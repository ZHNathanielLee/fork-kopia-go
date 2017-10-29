package repo

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/kopia/kopia/auth"
	"github.com/kopia/kopia/internal/config"
	"github.com/kopia/kopia/storage"
)

const (
	formatBlockID           = "format"
	repositoryConfigBlockID = "repo"
)

const (
	parallelFetches = 5
)

var (
	purposeAESKey   = []byte("AES")
	purposeAuthData = []byte("CHECKSUM")
)

// ErrMetadataNotFound is an error returned when a metadata item cannot be found.
var ErrMetadataNotFound = errors.New("metadata not found")

// SupportedMetadataEncryptionAlgorithms is a list of supported metadata encryption algorithms including:
//
//   AES256_GCM    - AES-256 in GCM mode
//   NONE          - no encryption
var SupportedMetadataEncryptionAlgorithms []string

// DefaultMetadataEncryptionAlgorithm is a metadata encryption algorithm used for new repositories.
const DefaultMetadataEncryptionAlgorithm = "AES256_GCM"

func init() {
	SupportedMetadataEncryptionAlgorithms = []string{
		"AES256_GCM",
		"NONE",
	}
}

// MetadataManager manages JSON metadata, such as snapshot manifests, policies, object format etc.
// in a repository.
type MetadataManager struct {
	storage storage.Storage
	cache   *metadataCache

	aead     cipher.AEAD // authenticated encryption to use
	authData []byte      // additional data to authenticate
}

// Put saves the specified metadata content under a provided name.
func (mm *MetadataManager) Put(itemID string, content []byte) error {
	if err := checkReservedName(itemID); err != nil {
		return err
	}

	return mm.writeEncryptedBlock(itemID, content)
}

// RefreshCache refreshes the cache of metadata items.
func (mm *MetadataManager) RefreshCache() error {
	return mm.cache.refresh()
}

func (mm *MetadataManager) writeEncryptedBlock(itemID string, content []byte) error {
	if mm.aead != nil {
		nonceLength := mm.aead.NonceSize()
		noncePlusContentLength := nonceLength + len(content)
		cipherText := make([]byte, noncePlusContentLength+mm.aead.Overhead())

		// Store nonce at the beginning of ciphertext.
		nonce := cipherText[0:nonceLength]
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			return err
		}

		b := mm.aead.Seal(cipherText[nonceLength:nonceLength], nonce, content, mm.authData)

		content = nonce[0 : nonceLength+len(b)]
	}

	return mm.cache.PutBlock(itemID, content)
}

func (mm *MetadataManager) readEncryptedBlock(itemID string) ([]byte, error) {
	content, err := mm.cache.GetBlock(itemID)
	if err != nil {
		if err == storage.ErrBlockNotFound {
			return nil, ErrMetadataNotFound
		}
		return nil, fmt.Errorf("unexpected error reading %v: %v", itemID, err)
	}

	return mm.decryptBlock(content)
}

func (mm *MetadataManager) decryptBlock(content []byte) ([]byte, error) {
	if mm.aead != nil {
		nonce := content[0:mm.aead.NonceSize()]
		payload := content[mm.aead.NonceSize():]
		return mm.aead.Open(payload[:0], nonce, payload, mm.authData)
	}

	return content, nil
}

// GetMetadata returns the contents of a specified metadata item.
func (mm *MetadataManager) GetMetadata(itemID string) ([]byte, error) {
	if err := checkReservedName(itemID); err != nil {
		return nil, err
	}

	return mm.readEncryptedBlock(itemID)
}

// MultiGet gets the contents of a specified multiple metadata items efficiently.
// The results are returned as a map, with items that are not found not present in the map.
func (mm *MetadataManager) MultiGet(itemIDs []string) (map[string][]byte, error) {
	type singleReadResult struct {
		id       string
		contents []byte
		err      error
	}

	ch := make(chan singleReadResult)
	inputs := make(chan string)
	for i := 0; i < parallelFetches; i++ {
		go func() {
			for itemID := range inputs {
				v, err := mm.GetMetadata(itemID)
				ch <- singleReadResult{itemID, v, err}
			}
		}()
	}

	go func() {
		// feed item IDs to workers.
		for _, i := range itemIDs {
			inputs <- i
		}
		close(inputs)
	}()

	// fetch exactly N results
	var resultErr error
	resultMap := make(map[string][]byte)
	for i := 0; i < len(itemIDs); i++ {
		r := <-ch
		if r.err != nil {
			resultErr = r.err
		} else {
			resultMap[r.id] = r.contents
		}
	}

	if resultErr != nil {
		return nil, resultErr
	}

	return resultMap, nil
}

func (mm *MetadataManager) getJSON(itemID string, content interface{}) error {
	j, err := mm.readEncryptedBlock(itemID)
	if err != nil {
		return err
	}

	return json.Unmarshal(j, content)
}

// putJSON stores the contents of an item stored with a given ID.
func (mm *MetadataManager) putJSON(id string, content interface{}) error {
	j, err := json.Marshal(content)
	if err != nil {
		return err
	}

	return mm.writeEncryptedBlock(id, j)
}

// List returns the list of metadata items matching the specified prefix.
func (mm *MetadataManager) List(prefix string) ([]string, error) {
	return mm.cache.ListBlocks(prefix)
}

// ListContents retrieves metadata contents for all items starting with a given prefix.
func (mm *MetadataManager) ListContents(prefix string) (map[string][]byte, error) {
	itemIDs, err := mm.List(prefix)
	if err != nil {
		return nil, err
	}

	return mm.MultiGet(itemIDs)
}

// Remove removes the specified metadata item.
func (mm *MetadataManager) Remove(itemID string) error {
	if err := checkReservedName(itemID); err != nil {
		return err
	}

	return mm.cache.DeleteBlock(itemID)
}

// RemoveMany efficiently removes multiple metadata items in parallel.
func (mm *MetadataManager) RemoveMany(itemIDs []string) error {
	parallelism := 30
	ch := make(chan string)
	var wg sync.WaitGroup
	errch := make(chan error, len(itemIDs))

	for i := 0; i < parallelism; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for id := range ch {
				if err := mm.Remove(id); err != nil {
					errch <- err
				}
			}
		}()
	}

	for _, id := range itemIDs {
		ch <- id
	}
	close(ch)
	wg.Wait()
	close(errch)

	return <-errch
}

// newMetadataManager opens a MetadataManager for given storage and credentials.
func newMetadataManager(st storage.Storage, f *config.MetadataFormat, km *auth.KeyManager) (*MetadataManager, error) {
	cache, err := newMetadataCache(st)
	if err != nil {
		return nil, err
	}

	mm := MetadataManager{
		storage: st,
		cache:   cache,
	}

	if err := mm.initCrypto(f, km); err != nil {
		return nil, fmt.Errorf("unable to initialize crypto: %v", err)
	}

	return &mm, nil
}

func (mm *MetadataManager) initCrypto(f *config.MetadataFormat, km *auth.KeyManager) error {
	switch f.EncryptionAlgorithm {
	case "NONE": // do nothing
		return nil
	case "AES256_GCM":
		aesKey := km.DeriveKey(purposeAESKey, 32)
		mm.authData = km.DeriveKey(purposeAuthData, 32)

		blk, err := aes.NewCipher(aesKey)
		if err != nil {
			return fmt.Errorf("cannot create cipher: %v", err)
		}
		mm.aead, err = cipher.NewGCM(blk)
		if err != nil {
			return fmt.Errorf("cannot create cipher: %v", err)
		}
		return nil
	default:
		return fmt.Errorf("unknown encryption algorithm: '%v'", f.EncryptionAlgorithm)
	}
}

func isReservedName(itemID string) bool {
	switch itemID {
	case formatBlockID, repositoryConfigBlockID:
		return true

	default:
		return false
	}
}

func checkReservedName(itemID string) error {
	if isReservedName(itemID) {
		return fmt.Errorf("invalid metadata item name: '%v'", itemID)
	}

	return nil
}
