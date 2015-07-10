package client

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/notary/client/changelist"
	"github.com/docker/notary/trustmanager"
	"github.com/endophage/gotuf"
	tufclient "github.com/endophage/gotuf/client"
	"github.com/endophage/gotuf/data"
	"github.com/endophage/gotuf/keys"
	"github.com/endophage/gotuf/signed"
	"github.com/endophage/gotuf/store"
)

// ErrRepoNotInitialized is returned when trying to can publish on an uninitialized
// notary repository
type ErrRepoNotInitialized struct{}

type passwordRetriever func() (string, error)

// ErrRepoNotInitialized is returned when trying to can publish on an uninitialized
// notary repository
func (err *ErrRepoNotInitialized) Error() string {
	return "Repository has not been initialized"
}

// Default paths should end with a '/' so directory creation works correctly
const (
	trustDir       string = "/trusted_certificates/"
	privDir        string = "/private/"
	tufDir         string = "/tuf/"
	rootKeysDir    string = privDir + "/root_keys/"
	rsaKeySize     int    = 2048 // Used for snapshots and targets keys
	rsaRootKeySize int    = 4096 // Used for new root keys
)

// ErrRepositoryNotExist gets returned when trying to make an action over a repository
/// that doesn't exist.
var ErrRepositoryNotExist = errors.New("repository does not exist")

// UnlockedSigner encapsulates a private key and a signer that uses that private key,
// providing convinience methods for generation of certificates.
type UnlockedSigner struct {
	privKey *data.PrivateKey
	signer  *signed.Signer
}

// NotaryRepository stores all the information needed to operate on a notary
// repository.
type NotaryRepository struct {
	baseDir          string
	gun              string
	baseURL          string
	tufRepoPath      string
	caStore          trustmanager.X509Store
	certificateStore trustmanager.X509Store
	fileStore        store.MetadataStore
	signer           *signed.Signer
	tufRepo          *tuf.TufRepo
	privKeyStore     *trustmanager.KeyFileStore
	rootKeyStore     *trustmanager.KeyFileStore
	rootSigner       *UnlockedSigner
	roundTrip        http.RoundTripper
}

// Target represents a simplified version of the data TUF operates on, so external
// applications don't have to depend on tuf data types.
type Target struct {
	Name   string
	Hashes data.Hashes
	Length int64
}

// NewTarget  is a helper method that returns a Target
func NewTarget(targetName string, targetPath string) (*Target, error) {
	b, err := ioutil.ReadFile(targetPath)
	if err != nil {
		return nil, err
	}

	meta, err := data.NewFileMeta(bytes.NewBuffer(b))
	if err != nil {
		return nil, err
	}

	return &Target{Name: targetName, Hashes: meta.Hashes, Length: meta.Length}, nil
}

// NewNotaryRepository is a helper method that returns a new notary repository.
// It takes the base directory under where all the trust files will be stored
// (usually ~/.docker/trust/).
func NewNotaryRepository(baseDir, gun, baseURL string, rt http.RoundTripper) (*NotaryRepository, error) {
	trustDir := filepath.Join(baseDir, trustDir)
	rootKeysDir := filepath.Join(baseDir, rootKeysDir)

	privKeyStore, err := trustmanager.NewKeyFileStore(filepath.Join(baseDir, privDir))
	if err != nil {
		return nil, err
	}

	fmt.Println("creating non-root cryptoservice")
	signer := signed.NewSigner(NewRSACryptoService(gun, privKeyStore, ""))

	nRepo := &NotaryRepository{
		gun:          gun,
		baseDir:      baseDir,
		baseURL:      baseURL,
		tufRepoPath:  filepath.Join(baseDir, tufDir, gun),
		signer:       signer,
		privKeyStore: privKeyStore,
		roundTrip:    rt,
	}

	if err := nRepo.loadKeys(trustDir, rootKeysDir); err != nil {
		return nil, err
	}

	return nRepo, nil
}

// Initialize creates a new repository by using rootKey as the root Key for the
// TUF repository.
func (r *NotaryRepository) Initialize(uSigner *UnlockedSigner) error {
	rootCert, err := uSigner.GenerateCertificate(r.gun)
	if err != nil {
		return err
	}
	r.certificateStore.AddCert(rootCert)
	rootKey := data.NewPublicKey("RSA", trustmanager.CertToPEM(rootCert))
	err = r.rootKeyStore.Link(uSigner.ID(), rootKey.ID())
	if err != nil {
		return err
	}

	remote, err := getRemoteStore(r.baseURL, r.gun, r.roundTrip)
	rawTSKey, err := remote.GetKey("timestamp")
	if err != nil {
		return err
	}

	parsedKey := &data.TUFKey{}
	err = json.Unmarshal(rawTSKey, parsedKey)
	if err != nil {
		return err
	}

	timestampKey := data.NewPublicKey(parsedKey.Cipher(), parsedKey.Public())

	targetsKey, err := r.signer.Create("targets")
	if err != nil {
		return err
	}
	snapshotKey, err := r.signer.Create("snapshot")
	if err != nil {
		return err
	}

	kdb := keys.NewDB()

	kdb.AddKey(rootKey)
	kdb.AddKey(targetsKey)
	kdb.AddKey(snapshotKey)
	kdb.AddKey(timestampKey)

	rootRole, err := data.NewRole("root", 1, []string{rootKey.ID()}, nil, nil)
	if err != nil {
		return err
	}
	targetsRole, err := data.NewRole("targets", 1, []string{targetsKey.ID()}, nil, nil)
	if err != nil {
		return err
	}
	snapshotRole, err := data.NewRole("snapshot", 1, []string{snapshotKey.ID()}, nil, nil)
	if err != nil {
		return err
	}
	timestampRole, err := data.NewRole("timestamp", 1, []string{timestampKey.ID()}, nil, nil)
	if err != nil {
		return err
	}

	if err := kdb.AddRole(rootRole); err != nil {
		return err
	}
	if err := kdb.AddRole(targetsRole); err != nil {
		return err
	}
	if err := kdb.AddRole(snapshotRole); err != nil {
		return err
	}
	if err := kdb.AddRole(timestampRole); err != nil {
		return err
	}

	r.tufRepo = tuf.NewTufRepo(kdb, r.signer)

	r.fileStore, err = store.NewFilesystemStore(
		r.tufRepoPath,
		"metadata",
		"json",
		"targets",
	)
	if err != nil {
		return err
	}

	if err := r.tufRepo.InitRepo(false); err != nil {
		return err
	}

	if err := r.saveMetadata(uSigner.signer); err != nil {
		return err
	}

	// Creates an empty snapshot
	return r.snapshot()
}

// AddTarget adds a new target to the repository, forcing a timestamps check from TUF
func (r *NotaryRepository) AddTarget(target *Target) error {
	cl, err := changelist.NewFileChangelist(filepath.Join(r.tufRepoPath, "changelist"))
	if err != nil {
		return err
	}
	fmt.Printf("Adding target \"%s\" with sha256 \"%s\" and size %d bytes.\n", target.Name, target.Hashes["sha256"], target.Length)

	meta := data.FileMeta{Length: target.Length, Hashes: target.Hashes}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	c := changelist.NewTufChange(changelist.ActionCreate, "targets", "target", target.Name, metaJSON)
	err = cl.Add(c)
	if err != nil {
		return err
	}
	return cl.Close()
}

// ListTargets lists all targets for the current repository
func (r *NotaryRepository) ListTargets() ([]*Target, error) {
	//r.bootstrapRepo()

	c, err := r.bootstrapClient()
	if err != nil {
		return nil, err
	}

	err = c.Update()
	if err != nil {
		return nil, err
	}

	var targetList []*Target
	for name, meta := range r.tufRepo.Targets["targets"].Signed.Targets {
		target := &Target{Name: name, Hashes: meta.Hashes, Length: meta.Length}
		targetList = append(targetList, target)
	}

	return targetList, nil
}

// GetTargetByName returns a target given a name
func (r *NotaryRepository) GetTargetByName(name string) (*Target, error) {
	//r.bootstrapRepo()

	c, err := r.bootstrapClient()
	if err != nil {
		return nil, err
	}

	err = c.Update()
	if err != nil {
		return nil, err
	}

	meta := c.TargetMeta(name)
	if meta == nil {
		return nil, errors.New("Meta is nil for target")
	}

	return &Target{Name: name, Hashes: meta.Hashes, Length: meta.Length}, nil
}

// Publish pushes the local changes in signed material to the remote notary-server
// Conceptually it performs an operation similar to a `git rebase`
func (r *NotaryRepository) Publish(getPass passwordRetriever) error {
	var updateRoot bool
	var root *data.Signed
	// attempt to initialize the repo from the remote store
	c, err := r.bootstrapClient()
	if err != nil {
		if _, ok := err.(*store.ErrMetaNotFound); ok {
			// if the remote store return a 404 (translated into ErrMetaNotFound),
			// the repo hasn't been initialized yet. Attempt to load it from disk.
			err := r.bootstrapRepo()
			if err != nil {
				// Repo hasn't been initialized, It must be initialized before
				// it can be published. Return an error and let caller determine
				// what it wants to do.
				logrus.Debug("Repository not initialized during Publish")
				return &ErrRepoNotInitialized{}
			}
			// We had local data but the server doesn't know about the repo yet,
			// ensure we will push the initial root file
			root, err = r.tufRepo.Root.ToSigned()
			if err != nil {
				return err
			}
			updateRoot = true
		} else {
			// The remote store returned an error other than 404. We're
			// unable to determine if the repo has been initialized or not.
			logrus.Error("Could not publish Repository: ", err.Error())
			return err
		}
	} else {
		// If we were successfully able to bootstrap the client (which only pulls
		// root.json), update it the rest of the tuf metadata in preparation for
		// applying the changelist.
		err = c.Update()
		if err != nil {
			return err
		}
	}

	// load the changelist for this repo
	cl, err := changelist.NewFileChangelist(filepath.Join(r.tufRepoPath, "changelist"))
	if err != nil {
		logrus.Debug("Error initializing changelist")
		return err
	}
	// apply the changelist to the repo
	err = applyChangelist(r.tufRepo, cl)
	if err != nil {
		logrus.Debug("Error applying changelist")
		return err
	}

	// check if our root file is nearing expiry. Resign if it is.
	if nearExpiry(r.tufRepo.Root) || r.tufRepo.Root.Dirty {
		passphrase, err := getPass()
		if err != nil {
			return err
		}
		rootKeyID := r.tufRepo.Root.Signed.Roles["root"].KeyIDs[0]
		rootSigner, err := r.GetRootSigner(rootKeyID, passphrase)
		if err != nil {
			return err
		}
		root, err = r.tufRepo.SignRoot(data.DefaultExpires("root"), rootSigner.signer)
		if err != nil {
			return err
		}
		updateRoot = true
	}
	// we will always resign targets and snapshots
	targets, err := r.tufRepo.SignTargets("targets", data.DefaultExpires("targets"), nil)
	if err != nil {
		return err
	}
	snapshot, err := r.tufRepo.SignSnapshot(data.DefaultExpires("snapshot"), nil)
	if err != nil {
		return err
	}

	remote, err := getRemoteStore(r.baseURL, r.gun, r.roundTrip)
	if err != nil {
		return err
	}

	// ensure we can marshal all the json before sending anything to remote
	targetsJSON, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}

	// if we need to update the root, marshal it and push the update to remote
	if updateRoot {
		rootJSON, err := json.Marshal(root)
		if err != nil {
			return err
		}
		err = remote.SetMeta("root", rootJSON)
		if err != nil {
			return err
		}
	}
	err = remote.SetMeta("targets", targetsJSON)
	if err != nil {
		return err
	}
	err = remote.SetMeta("snapshot", snapshotJSON)
	if err != nil {
		return err
	}

	return nil
}

func (r *NotaryRepository) bootstrapRepo() error {
	fileStore, err := store.NewFilesystemStore(
		r.tufRepoPath,
		"metadata",
		"json",
		"targets",
	)
	if err != nil {
		return err
	}

	kdb := keys.NewDB()
	tufRepo := tuf.NewTufRepo(kdb, r.signer)

	logrus.Debugf("Loading trusted collection.")
	rootJSON, err := fileStore.GetMeta("root", 0)
	if err != nil {
		return err
	}
	root := &data.Signed{}
	err = json.Unmarshal(rootJSON, root)
	if err != nil {
		return err
	}
	tufRepo.SetRoot(root)
	targetsJSON, err := fileStore.GetMeta("targets", 0)
	if err != nil {
		return err
	}
	targets := &data.Signed{}
	err = json.Unmarshal(targetsJSON, targets)
	if err != nil {
		return err
	}
	tufRepo.SetTargets("targets", targets)
	snapshotJSON, err := fileStore.GetMeta("snapshot", 0)
	if err != nil {
		return err
	}
	snapshot := &data.Signed{}
	err = json.Unmarshal(snapshotJSON, snapshot)
	if err != nil {
		return err
	}
	tufRepo.SetSnapshot(snapshot)

	r.tufRepo = tufRepo
	r.fileStore = fileStore

	return nil
}

func (r *NotaryRepository) saveMetadata(rootSigner *signed.Signer) error {
	signedRoot, err := r.tufRepo.SignRoot(data.DefaultExpires("root"), rootSigner)
	if err != nil {
		return err
	}

	rootJSON, _ := json.Marshal(signedRoot)
	return r.fileStore.SetMeta("root", rootJSON)
}

func (r *NotaryRepository) snapshot() error {
	logrus.Debugf("Saving changes to Trusted Collection.")

	for t := range r.tufRepo.Targets {
		signedTargets, err := r.tufRepo.SignTargets(t, data.DefaultExpires("targets"), nil)
		if err != nil {
			return err
		}
		targetsJSON, _ := json.Marshal(signedTargets)
		parentDir := filepath.Dir(t)
		os.MkdirAll(parentDir, 0755)
		r.fileStore.SetMeta(t, targetsJSON)
	}

	signedSnapshot, err := r.tufRepo.SignSnapshot(data.DefaultExpires("snapshot"), nil)
	if err != nil {
		return err
	}
	snapshotJSON, _ := json.Marshal(signedSnapshot)

	return r.fileStore.SetMeta("snapshot", snapshotJSON)
}

/*
validateRoot iterates over every root key included in the TUF data and attempts
to validate the certificate by first checking for an exact match on the certificate
store, and subsequently trying to find a valid chain on the caStore.

Example TUF Content for root role:
"roles" : {
  "root" : {
    "threshold" : 1,
      "keyids" : [
        "e6da5c303d572712a086e669ecd4df7b785adfc844e0c9a7b1f21a7dfc477a38"
      ]
  },
 ...
}

Example TUF Content for root key:
"e6da5c303d572712a086e669ecd4df7b785adfc844e0c9a7b1f21a7dfc477a38" : {
	"keytype" : "RSA",
	"keyval" : {
	  "private" : "",
	  "public" : "Base64-encoded, PEM encoded x509 Certificate"
	}
}
*/
func (r *NotaryRepository) validateRoot(root *data.Signed) error {
	rootSigned := &data.Root{}
	err := json.Unmarshal(root.Signed, rootSigned)
	if err != nil {
		return err
	}

	certs := make(map[string]*data.PublicKey)
	for _, fingerprint := range rootSigned.Roles["root"].KeyIDs {
		// TODO(dlaw): currently assuming only one cert contained in
		// public key entry. Need to fix when we want to pass in chains.
		k, _ := pem.Decode([]byte(rootSigned.Keys[fingerprint].Public()))
		logrus.Debug("Root PEM: ", k)
		logrus.Debug("Root ID: ", fingerprint)
		decodedCerts, err := x509.ParseCertificates(k.Bytes)
		if err != nil {
			continue
		}
		// TODO(diogo): Assuming that first certificate is the leaf-cert. Need to
		// iterate over all decodedCerts and find a non-CA one (should be the last).
		leafCert := decodedCerts[0]

		leafID := trustmanager.FingerprintCert(leafCert)

		// Check to see if there is an exact match of this certificate.
		// Checking the CommonName is not required since ID is calculated over
		// Cert.Raw. It's included to prevent breaking logic with changes of how the
		// ID gets computed.
		_, err = r.certificateStore.GetCertificateByFingerprint(leafID)
		if err == nil && leafCert.Subject.CommonName == r.gun {
			certs[fingerprint] = rootSigned.Keys[fingerprint]
		}

		// Check to see if this leafCertificate has a chain to one of the Root CAs
		// of our CA Store.
		certList := []*x509.Certificate{leafCert}
		err = trustmanager.Verify(r.caStore, r.gun, certList)
		if err == nil {
			certs[fingerprint] = rootSigned.Keys[fingerprint]
		}
	}

	if len(certs) < 1 {
		return errors.New("could not validate the path to a trusted root")
	}

	_, err = signed.VerifyRoot(root, 0, certs, 1)

	return err
}

func (r *NotaryRepository) bootstrapClient() (*tufclient.Client, error) {
	remote, err := getRemoteStore(r.baseURL, r.gun, r.roundTrip)
	if err != nil {
		return nil, err
	}
	rootJSON, err := remote.GetMeta("root", 5<<20)
	if err != nil {
		return nil, err
	}
	root := &data.Signed{}
	err = json.Unmarshal(rootJSON, root)
	if err != nil {
		return nil, err
	}

	err = r.validateRoot(root)
	if err != nil {
		return nil, err
	}

	kdb := keys.NewDB()
	r.tufRepo = tuf.NewTufRepo(kdb, r.signer)

	err = r.tufRepo.SetRoot(root)
	if err != nil {
		return nil, err
	}

	return tufclient.NewClient(
		r.tufRepo,
		remote,
		kdb,
	), nil
}

// ListRootKeys returns the IDs for all of the root keys. It ignores symlinks
// if any exist.
func (r *NotaryRepository) ListRootKeys() []string {
	return r.rootKeyStore.ListKeys()
}

// GenRootKey generates a new root key protected by a given passphrase
func (r *NotaryRepository) GenRootKey(passphrase string) (string, error) {
	privKey, err := trustmanager.GenerateRSAKey(rand.Reader, rsaRootKeySize)
	if err != nil {
		return "", fmt.Errorf("failed to convert private key: %v", err)
	}

	r.rootKeyStore.AddEncryptedKey(privKey.ID(), privKey, passphrase)

	return privKey.ID(), nil
}

// GetRootSigner retreives a root key that includes the ID and a signer
func (r *NotaryRepository) GetRootSigner(rootKeyID, passphrase string) (*UnlockedSigner, error) {
	privKey, err := r.rootKeyStore.GetDecryptedKey(rootKeyID, passphrase)
	if err != nil {
		return nil, fmt.Errorf("could not get decrypted root key: %v", err)
	}

	// This signer will be used for all of the normal TUF operations, except for
	// when a root key is needed.

	// Passing an empty GUN because root keys aren't associated with a GUN.
	fmt.Println("creating root cryptoservice with passphrase", passphrase)
	ccs := NewRSACryptoService("", r.rootKeyStore, passphrase)
	signer := signed.NewSigner(ccs)

	return &UnlockedSigner{
		privKey: privKey,
		signer:  signer}, nil
}

func (r *NotaryRepository) loadKeys(trustDir, rootKeysDir string) error {
	// Load all CAs that aren't expired and don't use SHA1
	caStore, err := trustmanager.NewX509FilteredFileStore(trustDir, func(cert *x509.Certificate) bool {
		return cert.IsCA && cert.BasicConstraintsValid && cert.SubjectKeyId != nil &&
			time.Now().Before(cert.NotAfter) &&
			cert.SignatureAlgorithm != x509.SHA1WithRSA &&
			cert.SignatureAlgorithm != x509.DSAWithSHA1 &&
			cert.SignatureAlgorithm != x509.ECDSAWithSHA1
	})
	if err != nil {
		return err
	}

	// Load all individual (non-CA) certificates that aren't expired and don't use SHA1
	certificateStore, err := trustmanager.NewX509FilteredFileStore(trustDir, func(cert *x509.Certificate) bool {
		return !cert.IsCA &&
			time.Now().Before(cert.NotAfter) &&
			cert.SignatureAlgorithm != x509.SHA1WithRSA &&
			cert.SignatureAlgorithm != x509.DSAWithSHA1 &&
			cert.SignatureAlgorithm != x509.ECDSAWithSHA1
	})
	if err != nil {
		return err
	}

	// Load the keystore that will hold all of our encrypted Root Private Keys
	rootKeyStore, err := trustmanager.NewKeyFileStore(rootKeysDir)
	if err != nil {
		return err
	}

	r.caStore = caStore
	r.certificateStore = certificateStore
	r.rootKeyStore = rootKeyStore

	return nil
}

// ID gets a consistent ID based on the PrivateKey bytes and cipher type
func (uk *UnlockedSigner) ID() string {
	return uk.PublicKey().ID()
}

// PublicKey Returns the public key associated with the Private Key within the Signer
func (uk *UnlockedSigner) PublicKey() *data.PublicKey {
	return data.PublicKeyFromPrivate(*uk.privKey)
}

// GenerateCertificate generates an X509 Certificate from a template, given a GUN
func (uk *UnlockedSigner) GenerateCertificate(gun string) (*x509.Certificate, error) {
	privKey, err := x509.ParsePKCS1PrivateKey(uk.privKey.Private())
	if err != nil {
		return nil, fmt.Errorf("failed to parse root key: %v (%s)", gun, err.Error())
	}

	template, err := trustmanager.NewCertificate(gun)
	if err != nil {
		return nil, fmt.Errorf("failed to create the certificate template for: %s (%v)", gun, err)
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, privKey.Public(), privKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create the certificate for: %s (%v)", gun, err)
	}

	// Encode the new certificate into PEM
	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse the certificate for key: %s (%v)", gun, err)
	}

	return cert, nil
}
