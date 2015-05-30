package trustmanager

import (
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
)

// X509FileStore implements X509Store that persists on disk
type X509FileStore struct {
	baseDir        string
	validate       Validator
	fileMap        map[ID]string
	fingerprintMap map[ID]*x509.Certificate
	nameMap        map[string][]ID
}

// NewX509FileStore returns a new X509FileStore.
func NewX509FileStore(directory string) *X509FileStore {
	validate := ValidatorFunc(func(cert *x509.Certificate) bool { return true })

	s := &X509FileStore{

		baseDir:        directory,
		validate:       validate,
		fileMap:        make(map[ID]string),
		fingerprintMap: make(map[ID]*x509.Certificate),
		nameMap:        make(map[string][]ID),
	}

	loadCertsFromDir(s, directory)

	return s
}

// NewX509FilteredFileStore returns a new X509FileStore that validates certificates
// that are added.
func NewX509FilteredFileStore(directory string, validate func(*x509.Certificate) bool) *X509FileStore {
	s := &X509FileStore{

		baseDir:        directory,
		validate:       ValidatorFunc(validate),
		fileMap:        make(map[ID]string),
		fingerprintMap: make(map[ID]*x509.Certificate),
		nameMap:        make(map[string][]ID),
	}

	loadCertsFromDir(s, directory)

	return s
}

// AddCert creates a filename for a given cert and adds a certificate with that name
func (s X509FileStore) AddCert(cert *x509.Certificate) error {
	if cert == nil {
		return errors.New("adding nil Certificate to X509Store")
	}

	fingerprint := fingerprintCert(cert)
	filename := path.Join(s.baseDir, string(fingerprint)+certExtension)
	if err := s.addNamedCert(cert, filename); err != nil {
		return err
	}

	return nil
}

// addNamedCert allows adding a certificate while controling the filename it gets
// stored under. If the file does not exist on disk, saves it.
func (s X509FileStore) addNamedCert(cert *x509.Certificate, filename string) error {
	if cert == nil {
		return errors.New("adding nil Certificate to X509Store")
	}

	fingerprint := fingerprintCert(cert)

	// Validate if we already loaded this certificate before
	if _, ok := s.fingerprintMap[fingerprint]; ok {
		return errors.New("certificate already in the store")
	}

	// Check if this certificate meets our validation criteria
	if !s.validate.Validate(cert) {
		return errors.New("certificate validation failed")
	}

	// Overwrite every certificate SubjectKeyID with a SHA256 version.
	subjectKeyID := sha256.Sum256(cert.Raw)
	cert.SubjectKeyId = subjectKeyID[:]

	// Add the certificate to our in-memory storage
	s.fingerprintMap[fingerprint] = cert
	s.fileMap[fingerprint] = filename

	name := string(cert.RawSubject)
	s.nameMap[name] = append(s.nameMap[name], fingerprint)

	// Save the file to disk if not already there.
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		return saveCertificate(cert, filename)
	}

	return nil
}

// RemoveCert removes a certificate from a X509FileStore.
func (s X509FileStore) RemoveCert(cert *x509.Certificate) error {
	if cert == nil {
		return errors.New("removing nil Certificate from X509Store")
	}

	fingerprint := fingerprintCert(cert)
	delete(s.fingerprintMap, fingerprint)
	filename := s.fileMap[fingerprint]
	delete(s.fileMap, fingerprint)

	name := string(cert.RawSubject)

	// Filter the fingerprint out of this name entry
	fpList := s.nameMap[name]
	newfpList := fpList[:0]
	for _, x := range fpList {
		if x != fingerprint {
			newfpList = append(newfpList, x)
		}
	}

	s.nameMap[name] = newfpList

	if err := os.Remove(filename); err != nil {
		return err
	}

	return nil
}

// AddCertFromPEM adds the first certificate that it finds in the byte[], returning
// an error if no Certificates are found
func (s X509FileStore) AddCertFromPEM(pemBytes []byte) error {
	cert, err := loadCertFromPEM(pemBytes)
	if err != nil {
		return err
	}
	return s.AddCert(cert)
}

// AddCertFromFile tries to adds a X509 certificate to the store given a filename
func (s X509FileStore) AddCertFromFile(originFilname string) error {
	cert, err := loadCertFromFile(originFilname)
	if err != nil {
		return err
	}

	filename := s.genDestinationCertFilename(cert, originFilname)

	return s.addNamedCert(cert, filename)
}

// AddCertFromURL tries to adds a X509 certificate to the store given a HTTPS URL
func (s X509FileStore) AddCertFromURL(urlStr string) error {
	url, err := url.Parse(urlStr)
	if err != nil {
		return err
	}

	// Check if we are adding via HTTPS
	if url.Scheme != "https" {
		return errors.New("only HTTPS URLs allowed.")
	}

	// Download the certificate and write to directory
	resp, err := http.Get(url.String())
	if err != nil {
		return err
	}

	// Copy the content to certBytes
	defer resp.Body.Close()
	certBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	// Try to extract the first valid PEM certificate from the bytes
	cert, err := loadCertFromPEM(certBytes)
	if err != nil {
		return err
	}

	// Generate a unique destination URL based on the path
	filename := s.genDestinationCertFilename(cert, url.Path)

	return s.addNamedCert(cert, filename)
}

// GetCertificates returns an array with all of the current X509 Certificates.
func (s X509FileStore) GetCertificates() []*x509.Certificate {
	certs := make([]*x509.Certificate, len(s.fingerprintMap))
	i := 0
	for _, v := range s.fingerprintMap {
		certs[i] = v
		i++
	}
	return certs
}

// GetCertificatePool returns an x509 CertPool loaded with all the certificates
// in the store.
func (s X509FileStore) GetCertificatePool() *x509.CertPool {
	pool := x509.NewCertPool()

	for _, v := range s.fingerprintMap {
		pool.AddCert(v)
	}
	return pool
}

// genDestinationCertFilename generates a unique destination certificate filename
// given a sourceFilename to help keep indication to where the original file came from
func (s X509FileStore) genDestinationCertFilename(cert *x509.Certificate, sourceFilename string) string {
	// Take the file name, extension and base name from filename
	_, fName := path.Split(sourceFilename)
	extName := path.Ext(sourceFilename)
	bName := fName[:len(fName)-len(extName)]

	filename := path.Join(s.baseDir, bName+certExtension)

	// If a file with the same name already exists in the destination directory
	// add hash to filename
	if _, err := os.Stat(filename); err == nil {
		fingerprint := fingerprintCert(cert)
		// Add the certificate fingerprint to the file basename_FINGERPRINT.crt
		filename = path.Join(s.baseDir, bName+"_"+string(fingerprint)+certExtension)
	}
	return filename
}

// GetCertificateBySKID returns the certificate that matches a certain SKID or error
func (s X509FileStore) GetCertificateBySKID(hexSKID string) (*x509.Certificate, error) {
	// If it does not look like a hex encoded sha256 hash, error
	if len(hexSKID) != 64 {
		return nil, errors.New("invalid Subject Key Identifier")
	}

	// Check to see if this subject key identifier exists
	if cert, ok := s.fingerprintMap[ID(hexSKID)]; ok {
		return cert, nil

	}
	return nil, errors.New("certificate not found in Key Store")
}
