package libkb

import (
	"fmt"
	keybase1 "github.com/keybase/client/go/protocol/keybase1"
	context "golang.org/x/net/context"
	"sync"
)

// SharedDHKeyGeneration describes which generation of DH key we're talking about.
// The sequence starts at 1, and should increment every time the shared DH key
// rotates, which is everytime a device is revoked.
type SharedDHKeyGeneration int

type SharedDHSecretKeyBox struct {
	Generation  SharedDHKeyGeneration `json:"generation"`
	Box         string                `json:"box"`
	ReceiverKID keybase1.KID          `json:"receiver_kid"`
}

type sharedDHSecretKeyBoxesResp struct {
	Boxes  []SharedDHSecretKeyBox `json:"boxes"`
	Status AppStatus              `json:"status"`
}

func (s *sharedDHSecretKeyBoxesResp) GetAppStatus() *AppStatus {
	return &s.Status
}

// SharedDHKeyMap is a map of Generation numbers to
// DH private keys, for decrypting data.
type SharedDHKeyMap map[SharedDHKeyGeneration]NaclDHKeyPair

// SharedDHKeyring holds on to all versions of the Shared DH key.
// Generation=0 should be nil, but all others should be present.
type SharedDHKeyring struct {
	Contextified
	sync.Mutex
	uid         keybase1.UID
	generations SharedDHKeyMap
}

// NewSharedDHKeyring makes a new SharedDH keyring for a given UID.
func NewSharedDHKeyring(g *GlobalContext, uid keybase1.UID) *SharedDHKeyring {
	return &SharedDHKeyring{
		Contextified: NewContextified(g),
		uid:          uid,
		generations:  make(SharedDHKeyMap),
	}
}

// CurrentGeneration returns what generation we're on. The version possible
// Version is 1. Version 0 implies no keys are available.
func (s *SharedDHKeyring) CurrentGeneration() SharedDHKeyGeneration {
	s.Lock()
	defer s.Unlock()
	return s.currentGenerationLocked()
}

func (s *SharedDHKeyring) currentGenerationLocked() SharedDHKeyGeneration {
	return SharedDHKeyGeneration(len(s.generations))
}

func (s *SharedDHKeyring) SharedDHKey(g SharedDHKeyGeneration) *NaclDHKeyPair {
	s.Lock()
	defer s.Unlock()
	key, found := s.generations[g]
	if !found {
		return nil
	}
	return &key
}

// Clone makes a deep copy of this DH keyring.
func (s *SharedDHKeyring) Clone() *SharedDHKeyring {
	s.Lock()
	defer s.Unlock()
	ret := NewSharedDHKeyring(s.G(), s.uid)
	ret.mergeLocked(s.generations)
	return ret
}

// Update will take the existing SharedDHKeyring, and return an updated
// copy, that will be synced with the server's version of our SharedDHKeyring.
func (s *SharedDHKeyring) Update(ctx context.Context) (ret *SharedDHKeyring, err error) {
	ret = s.Clone()
	err = ret.Sync(ctx)
	return ret, err
}

// Sync our SharedDHKeyring with the server. It will either add all new
// Secret boxes since our last update, or not at all if there was an error.
// Pass it a standard Go network context.
func (s *SharedDHKeyring) Sync(ctx context.Context) (err error) {
	defer s.G().CTrace(ctx, "SharedDHKeyring#Sync", func() error { return err })()

	s.Lock()
	defer s.Unlock()

	boxes, err := s.fetchBoxesLocked(ctx)
	if err != nil {
		return err
	}

	upak, _, err := s.G().GetUPAKLoader().Load(NewLoadUserByUIDArg(ctx, s.G(), s.uid))
	if err != nil {
		return err
	}

	newKeys, err := s.importLocked(ctx, boxes, newSharedDHChecker(upak))
	if err != nil {
		return err

	}
	s.mergeLocked(newKeys)
	return nil
}

func (s *SharedDHKeyring) mergeLocked(m SharedDHKeyMap) (err error) {
	for k, v := range m {
		s.generations[k] = v.Clone()
	}
	return nil
}

func (s *SharedDHKeyring) fetchBoxesLocked(ctx context.Context) (ret []SharedDHSecretKeyBox, err error) {
	defer s.G().CTrace(ctx, "SharedDHKeyring#fetchBoxesLocked", func() error { return err })()

	did := s.G().Env.GetDeviceIDForUID(s.uid)
	if did.IsNil() {
		return nil, DeviceRequiredError{}
	}

	var resp sharedDHSecretKeyBoxesResp
	err = s.G().API.GetDecode(APIArg{
		Endpoint: "key/fetch_shared_dh_secrets",
		Args: HTTPArgs{
			"generation": I{int(s.currentGenerationLocked())},
			"device_id":  S{did.String()},
		},
		SessionType: APISessionTypeREQUIRED,
		RetryCount:  5, // It's pretty bad to fail this, so retry.
		NetContext:  ctx,
	}, &resp)
	if err != nil {
		return nil, err
	}
	ret = resp.Boxes
	s.G().Log.CDebugf(ctx, "| Got back %d boxes from server", len(ret))
	return ret, nil
}

// sharedDHChecker checks the secret boxes returned from the server
// against the public keys advertised in the user's sigchain. As we import
// keys, we check them against these two maps.  In particular, we check that
// the box was encryted with a valid device Box key (though it can be now revoked).
// And we check that the public key corresponds to what was signed in as a
// shared_dh_key.
type sharedDHChecker struct {
	allowedEncryptingKIDs map[keybase1.KID]bool
	expectedSharedDHKIDs  map[SharedDHKeyGeneration]keybase1.KID
}

func newSharedDHChecker(upak *keybase1.UserPlusAllKeys) *sharedDHChecker {
	ret := sharedDHChecker{
		allowedEncryptingKIDs: make(map[keybase1.KID]bool),
		expectedSharedDHKIDs:  make(map[SharedDHKeyGeneration]keybase1.KID),
	}
	isEncryptionKey := func(k keybase1.PublicKey) bool {
		return !k.IsSibkey && k.PGPFingerprint == ""
	}
	for _, r := range upak.Base.RevokedDeviceKeys {
		if isEncryptionKey(r.Key) {
			ret.allowedEncryptingKIDs[r.Key.KID] = true
		}
	}
	for _, k := range upak.Base.DeviceKeys {
		if isEncryptionKey(k) {
			ret.allowedEncryptingKIDs[k.KID] = true
		}
	}
	for _, k := range upak.Base.SharedDHKeys {
		ret.expectedSharedDHKIDs[SharedDHKeyGeneration(k.Gen)] = k.Kid
	}
	return &ret
}

func importSharedDHKey(box *SharedDHSecretKeyBox, activeDecryptionKey GenericKey, wantedGeneration SharedDHKeyGeneration, checker *sharedDHChecker) (ret *NaclDHKeyPair, err error) {
	if box.Generation != wantedGeneration {
		return nil, SharedDHImportError{fmt.Sprintf("bad generation returned: %d", box.Generation)}
	}
	if !activeDecryptionKey.GetKID().Equal(box.ReceiverKID) {
		return nil, SharedDHImportError{fmt.Sprintf("wrong encryption kid: %s", box.ReceiverKID.String())}
	}
	rawKey, encryptingKID, err := activeDecryptionKey.DecryptFromString(box.Box)
	if err != nil {
		return nil, err
	}
	if !checker.allowedEncryptingKIDs[encryptingKID] {
		return nil, SharedDHImportError{fmt.Sprintf("unexpected encrypting kid: %s", encryptingKID)}
	}
	key, err := MakeNaclDHKeyPairFromSecretBytes(rawKey)
	if err != nil {
		return nil, err
	}
	expectedKID, found := checker.expectedSharedDHKIDs[box.Generation]
	if !found {
		return nil, SharedDHImportError{fmt.Sprintf("No known generation: %d", box.Generation)}
	}
	if !expectedKID.Equal(key.GetKID()) {
		return nil, SharedDHImportError{fmt.Sprintf("Wrong public key for gen=%d; %s != %s", box.Generation, expectedKID, key.GetKID())}
	}

	return &key, nil
}

func (s *SharedDHKeyring) importLocked(ctx context.Context, boxes []SharedDHSecretKeyBox, checker *sharedDHChecker) (ret SharedDHKeyMap, err error) {
	defer s.G().CTrace(ctx, "SharedDHKeyring#importLocked", func() error { return err })()

	ret = make(SharedDHKeyMap)
	var activeDecryptionKey GenericKey
	activeDecryptionKey, err = s.G().ActiveDevice.EncryptionKey()
	if err != nil {
		return nil, err
	}
	nxt := s.currentGenerationLocked() + 1
	for _, box := range boxes {
		naclDHKey, err := importSharedDHKey(&box, activeDecryptionKey, nxt, checker)
		if err != nil {
			return nil, err
		}
		ret[nxt] = *naclDHKey
		nxt++
	}
	return ret, nil
}
