package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	ctxu "github.com/docker/distribution/context"
	"github.com/gorilla/mux"
	"golang.org/x/net/context"

	"github.com/docker/notary/server/errors"
	"github.com/docker/notary/server/snapshot"
	"github.com/docker/notary/server/storage"
	"github.com/docker/notary/server/timestamp"
	"github.com/docker/notary/tuf/data"
	"github.com/docker/notary/tuf/signed"
	"github.com/docker/notary/tuf/validation"
	"github.com/docker/notary/utils"
)

// TUFResponse is a response struct with various data about a TUF file
type TUFResponse struct {
	Gun     string `json:"gun"`
	Role    string `json:"role"`
	Version int    `json:"version"`
	Sha256  string `json:"sha256"`
}

// MainHandler is the default handler for the server
func MainHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	// For now it only supports `GET`
	if r.Method != "GET" {
		return errors.ErrGenericNotFound.WithDetail(nil)
	}

	if _, err := w.Write([]byte("{}")); err != nil {
		return errors.ErrUnknown.WithDetail(err)
	}
	return nil
}

// AtomicUpdateHandler will accept multiple TUF files and ensure that the storage
// backend is atomically updated with all the new records.
func AtomicUpdateHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()
	vars := mux.Vars(r)
	return atomicUpdateHandler(ctx, w, r, vars)
}

func atomicUpdateHandler(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	gun := vars["imageName"]
	s := ctx.Value("metaStore")
	logger := ctxu.GetLoggerWithField(ctx, gun, "gun")
	store, ok := s.(storage.MetaStore)
	if !ok {
		logger.Error("500 POST unable to retrieve storage")
		return errors.ErrNoStorage.WithDetail(nil)
	}
	cryptoServiceVal := ctx.Value("cryptoService")
	cryptoService, ok := cryptoServiceVal.(signed.CryptoService)
	if !ok {
		logger.Error("500 POST unable to retrieve signing service")
		return errors.ErrNoCryptoService.WithDetail(nil)
	}

	reader, err := r.MultipartReader()
	if err != nil {
		logger.Info("400 POST unable to parse TUF data")
		return errors.ErrMalformedUpload.WithDetail(nil)
	}
	var updates []storage.MetaUpdate
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		role := strings.TrimSuffix(part.FileName(), ".json")
		if role == "" {
			logger.Info("400 POST empty role")
			return errors.ErrNoFilename.WithDetail(nil)
		} else if !data.ValidRole(role) {
			logger.Infof("400 POST invalid role: %s", role)
			return errors.ErrInvalidRole.WithDetail(role)
		}
		meta := &data.SignedMeta{}
		var input []byte
		inBuf := bytes.NewBuffer(input)
		dec := json.NewDecoder(io.TeeReader(part, inBuf))
		err = dec.Decode(meta)
		if err != nil {
			logger.Info("400 POST malformed update JSON")
			return errors.ErrMalformedJSON.WithDetail(nil)
		}
		version := meta.Signed.Version
		updates = append(updates, storage.MetaUpdate{
			Role:    role,
			Version: version,
			Data:    inBuf.Bytes(),
		})
	}
	updates, err = validateUpdate(cryptoService, gun, updates, store)
	if err != nil {
		serializable, serializableError := validation.NewSerializableError(err)
		if serializableError != nil {
			logger.Info("400 POST error validating update")
			return errors.ErrInvalidUpdate.WithDetail(nil)
		}
		return errors.ErrInvalidUpdate.WithDetail(serializable)
	}
	err = store.UpdateMany(gun, updates)
	if err != nil {
		// If we have an old version error, surface to user with error code
		if _, ok := err.(storage.ErrOldVersion); ok {
			logger.Info("400 POST old version error")
			return errors.ErrOldVersion.WithDetail(err)
		}
		// More generic storage update error, possibly due to attempted rollback
		logger.Errorf("500 POST error applying update request: %v", err)
		return errors.ErrUpdating.WithDetail(nil)
	}
	return nil
}

// GetHandler returns the json for a specified role and GUN.
func GetHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()
	vars := mux.Vars(r)
	return getHandler(ctx, w, r, vars)
}

func getHandler(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	gun := vars["imageName"]
	checksum := vars["checksum"]
	tufRole := vars["tufRole"]
	s := ctx.Value("metaStore")

	logger := ctxu.GetLoggerWithField(ctx, gun, "gun")

	store, ok := s.(storage.MetaStore)
	if !ok {
		logger.Error("500 GET: no storage exists")
		return errors.ErrNoStorage.WithDetail(nil)
	}

	lastModified, output, err := getRole(ctx, store, gun, tufRole, checksum)
	if err != nil {
		logger.Infof("404 GET %s role", tufRole)
		return err
	}
	if lastModified != nil {
		// This shouldn't always be true, but in case it is nil, and the last modified headers
		// are not set, the cache control handler should set the last modified date to the beginning
		// of time.
		utils.SetLastModifiedHeader(w.Header(), *lastModified)
	} else {
		logger.Warnf("Got bytes out for %s's %s (checksum: %s), but missing lastModified date",
			gun, tufRole, checksum)
	}

	w.Write(output)
	return nil
}

// DeleteHandler deletes all data for a GUN. A 200 responses indicates success.
func DeleteHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	vars := mux.Vars(r)
	gun := vars["imageName"]
	logger := ctxu.GetLoggerWithField(ctx, gun, "gun")
	s := ctx.Value("metaStore")
	store, ok := s.(storage.MetaStore)
	if !ok {
		logger.Error("500 DELETE repository: no storage exists")
		return errors.ErrNoStorage.WithDetail(nil)
	}
	err := store.Delete(gun)
	if err != nil {
		logger.Error("500 DELETE repository")
		return errors.ErrUnknown.WithDetail(err)
	}
	return nil
}

// GetKeyHandler returns a public key for the specified role, creating a new key-pair
// it if it doesn't yet exist
func GetKeyHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()
	vars := mux.Vars(r)
	return getKeyHandler(ctx, w, r, vars)
}

func getKeyHandler(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	gun, ok := vars["imageName"]
	logger := ctxu.GetLoggerWithField(ctx, gun, "gun")
	if !ok || gun == "" {
		logger.Info("400 GET no gun in request")
		return errors.ErrUnknown.WithDetail("no gun")
	}

	role, ok := vars["tufRole"]
	if !ok || role == "" {
		logger.Info("400 GET no role in request")
		return errors.ErrUnknown.WithDetail("no role")
	}

	s := ctx.Value("metaStore")
	store, ok := s.(storage.MetaStore)
	if !ok || store == nil {
		logger.Error("500 GET storage not configured")
		return errors.ErrNoStorage.WithDetail(nil)
	}
	c := ctx.Value("cryptoService")
	crypto, ok := c.(signed.CryptoService)
	if !ok || crypto == nil {
		logger.Error("500 GET crypto service not configured")
		return errors.ErrNoCryptoService.WithDetail(nil)
	}
	algo := ctx.Value("keyAlgorithm")
	keyAlgo, ok := algo.(string)
	if !ok || keyAlgo == "" {
		logger.Error("500 GET key algorithm not configured")
		return errors.ErrNoKeyAlgorithm.WithDetail(nil)
	}
	keyAlgorithm := keyAlgo

	var (
		key data.PublicKey
		err error
	)
	switch role {
	case data.CanonicalTimestampRole:
		key, err = timestamp.GetOrCreateTimestampKey(gun, store, crypto, keyAlgorithm)
	case data.CanonicalSnapshotRole:
		key, err = snapshot.GetOrCreateSnapshotKey(gun, store, crypto, keyAlgorithm)
	default:
		logger.Infof("400 GET %s key: %v", role, err)
		return errors.ErrInvalidRole.WithDetail(role)
	}
	if err != nil {
		logger.Errorf("500 GET %s key: %v", role, err)
		return errors.ErrUnknown.WithDetail(err)
	}

	out, err := json.Marshal(key)
	if err != nil {
		logger.Errorf("500 GET %s key", role)
		return errors.ErrUnknown.WithDetail(err)
	}
	logger.Debugf("200 GET %s key", role)
	w.Write(out)
	return nil
}

// QueryTUFFilesHandler returns all TUF Files that were created or modified between createdBefore and createdAfter query parameters.
// If createdBefore is not present, there is no upper bound on when modifications happened.
// If createdAfter is not present, there is no lower bound on when modifications happened.
func QueryTUFFilesHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var createdBefore, createdAfter *time.Time
	logger := ctxu.GetLogger(ctx)
	layout := "2006-01-02T15:04:05.000Z"
	createdBeforeString := r.URL.Query().Get("createdBefore")
	if createdBeforeString != "" {
		parsed, err := time.Parse(layout, createdBeforeString)
		if err != nil {
			return errors.ErrMalformedQueryParameters.WithDetail(fmt.Sprintf("Timing parameters should be formatted like '%s'. Please check createdBefore.", layout))
		}
		createdBefore = &parsed
	}
	createdAfterString := r.URL.Query().Get("createdAfter")
	if createdAfterString != "" {
		parsed, err := time.Parse(layout, createdAfterString)
		if err != nil {
			return errors.ErrMalformedQueryParameters.WithDetail(fmt.Sprintf("Timing parameters should be formatted like '%s'. Please check createdAfter.", layout))
		}
		createdAfter = &parsed
	}

	s := ctx.Value("metaStore")
	store, ok := s.(storage.MetaStore)
	if !ok {
		logger.Error("500 GET TUF Files: no storage exists")
		return errors.ErrNoStorage.WithDetail(nil)
	}
	tufFiles, err := store.GetAll(createdBefore, createdAfter)
	if err != nil {
		logger.Error("500 GET TUF Files")
		return errors.ErrUnknown.WithDetail(err)
	}

	var resp []TUFResponse
	for _, tufFile := range tufFiles {
		resp = append(resp, TUFResponse{
			Gun:     tufFile.GetGUN(),
			Role:    tufFile.GetRole(),
			Version: tufFile.GetVersion(),
			Sha256:  tufFile.GetSha256(),
		})
	}

	out, err := json.Marshal(tufFiles)
	if err != nil {
		logger.Error("500 GET TUF Files")
		return errors.ErrUnknown.WithDetail(err)
	}
	w.Write(out)

	return nil
}

// NotFoundHandler is used as a generic catch all handler to return the ErrMetadataNotFound
// 404 response
func NotFoundHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	return errors.ErrMetadataNotFound.WithDetail(nil)
}
