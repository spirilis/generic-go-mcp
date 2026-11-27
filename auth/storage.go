package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Storage defines the interface for persisting auth data
type Storage interface {
	// Authorization codes
	StoreAuthCode(ctx context.Context, code *AuthorizationCode) error
	GetAuthCode(ctx context.Context, code string) (*AuthorizationCode, error)
	DeleteAuthCode(ctx context.Context, code string) error

	// Access tokens
	StoreAccessToken(ctx context.Context, token *AccessToken) error
	GetAccessToken(ctx context.Context, token string) (*AccessToken, error)
	DeleteAccessToken(ctx context.Context, token string) error

	// Refresh tokens
	StoreRefreshToken(ctx context.Context, token *RefreshToken) error
	GetRefreshToken(ctx context.Context, token string) (*RefreshToken, error)
	DeleteRefreshToken(ctx context.Context, token string) error

	// Registered clients
	StoreClient(ctx context.Context, client *RegisteredClient) error
	GetClient(ctx context.Context, clientID string) (*RegisteredClient, error)
	DeleteClient(ctx context.Context, clientID string) error
	ListClients(ctx context.Context) ([]*RegisteredClient, error)

	// Users
	StoreUser(ctx context.Context, user *User) error
	GetUser(ctx context.Context, userID string) (*User, error)
	GetUserByGitHubLogin(ctx context.Context, login string) (*User, error)
	// GetUserByIdentity finds the user a provider identity was stored for (User.Provider,
	// Issuer and Subject). Records from before v0.12.0 are not indexed until their next login.
	GetUserByIdentity(ctx context.Context, provider, issuer, subject string) (*User, error)

	// Sessions
	StoreSession(ctx context.Context, session *AuthSession) error
	GetSession(ctx context.Context, sessionID string) (*AuthSession, error)
	GetSessionByAccessToken(ctx context.Context, token string) (*AuthSession, error)
	UpdateSessionLastUsed(ctx context.Context, sessionID string, lastUsed time.Time) error
	DeleteSession(ctx context.Context, sessionID string) error

	// Pending auth requests
	StoreAuthRequest(ctx context.Context, req *PendingAuthRequest) error
	GetAuthRequest(ctx context.Context, id string) (*PendingAuthRequest, error)
	DeleteAuthRequest(ctx context.Context, id string) error

	// Consent. A pending consent is an authorization that has passed the provider login and
	// the access rules and is waiting on the user to approve the client; TakePendingConsent claims it
	// exactly once, and only for the browser that holds the matching binding cookie.
	StorePendingConsent(ctx context.Context, pc *PendingConsent) error
	TakePendingConsent(ctx context.Context, id, bindingHash string) (*PendingConsent, error)
	StoreConsent(ctx context.Context, userID, clientID string) error
	HasConsent(ctx context.Context, userID, clientID string) (bool, error)

	// Close
	Close() error
}

// BoltStorage implements Storage using BoltDB
type BoltStorage struct {
	path string

	// writeGate is held shared by every write and exclusively by Compact for its whole run,
	// so the copy Compact makes cannot miss a write. dbMu guards the db handle itself: held
	// shared by every read and write, exclusively only while Compact swaps the file (and by
	// Close). Lock order is writeGate, then dbMu.
	writeGate sync.RWMutex
	dbMu      sync.RWMutex
	db        *bolt.DB

	// compactHook, when set by a test, runs after the copy and before the swap.
	compactHook func()
}

// view runs fn in a read transaction.
func (s *BoltStorage) view(fn func(*bolt.Tx) error) error {
	s.dbMu.RLock()
	defer s.dbMu.RUnlock()
	return s.db.View(fn)
}

// update runs fn in a write transaction. It waits while Compact runs.
func (s *BoltStorage) update(fn func(*bolt.Tx) error) error {
	s.writeGate.RLock()
	defer s.writeGate.RUnlock()
	s.dbMu.RLock()
	defer s.dbMu.RUnlock()
	return s.db.Update(fn)
}

func openBolt(path string) (*bolt.DB, error) {
	return bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
}

// Bucket names
const (
	BucketAuthCodes       = "auth_codes"
	BucketAccessTokens    = "access_tokens"
	BucketRefreshTokens   = "refresh_tokens"
	BucketClients         = "clients"
	BucketUsers           = "users"
	BucketUsersByGitHub   = "users_by_github"
	BucketUsersByIdentity = "users_by_identity"
	BucketSessions        = "sessions"
	BucketSessionsByToken = "sessions_by_token"
	BucketAuthRequests    = "auth_requests"
	BucketPendingConsents = "pending_consents"
	BucketConsents        = "consents"
)

// NewBoltStorage creates a new BoltDB storage
func NewBoltStorage(path string) (*BoltStorage, error) {
	db, err := openBolt(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open bolt db: %w", err)
	}

	// Create buckets
	err = db.Update(func(tx *bolt.Tx) error {
		buckets := []string{
			BucketAuthCodes, BucketAccessTokens, BucketRefreshTokens,
			BucketClients, BucketUsers, BucketUsersByGitHub, BucketUsersByIdentity,
			BucketSessions, BucketSessionsByToken, BucketAuthRequests,
			BucketPendingConsents, BucketConsents,
		}
		for _, bucket := range buckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(bucket)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create buckets: %w", err)
	}

	return &BoltStorage{path: path, db: db}, nil
}

// Close closes the database
func (s *BoltStorage) Close() error {
	s.writeGate.Lock()
	defer s.writeGate.Unlock()
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	return s.db.Close()
}

// Authorization Codes
func (s *BoltStorage) StoreAuthCode(ctx context.Context, code *AuthorizationCode) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAuthCodes))
		data, err := json.Marshal(code)
		if err != nil {
			return err
		}
		return b.Put([]byte(code.Code), data)
	})
}

func (s *BoltStorage) GetAuthCode(ctx context.Context, code string) (*AuthorizationCode, error) {
	var authCode *AuthorizationCode
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAuthCodes))
		data := b.Get([]byte(code))
		if data == nil {
			return ErrTokenNotFound
		}
		authCode = &AuthorizationCode{}
		return json.Unmarshal(data, authCode)
	})
	return authCode, err
}

func (s *BoltStorage) DeleteAuthCode(ctx context.Context, code string) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAuthCodes))
		return b.Delete([]byte(code))
	})
}

// Access Tokens
func (s *BoltStorage) StoreAccessToken(ctx context.Context, token *AccessToken) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAccessTokens))
		data, err := json.Marshal(token)
		if err != nil {
			return err
		}
		return b.Put([]byte(token.Token), data)
	})
}

func (s *BoltStorage) GetAccessToken(ctx context.Context, token string) (*AccessToken, error) {
	var accessToken *AccessToken
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAccessTokens))
		data := b.Get([]byte(token))
		if data == nil {
			return ErrTokenNotFound
		}
		accessToken = &AccessToken{}
		return json.Unmarshal(data, accessToken)
	})
	return accessToken, err
}

func (s *BoltStorage) DeleteAccessToken(ctx context.Context, token string) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAccessTokens))
		return b.Delete([]byte(token))
	})
}

// Refresh Tokens
func (s *BoltStorage) StoreRefreshToken(ctx context.Context, token *RefreshToken) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketRefreshTokens))
		data, err := json.Marshal(token)
		if err != nil {
			return err
		}
		return b.Put([]byte(token.Token), data)
	})
}

func (s *BoltStorage) GetRefreshToken(ctx context.Context, token string) (*RefreshToken, error) {
	var refreshToken *RefreshToken
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketRefreshTokens))
		data := b.Get([]byte(token))
		if data == nil {
			return ErrTokenNotFound
		}
		refreshToken = &RefreshToken{}
		return json.Unmarshal(data, refreshToken)
	})
	return refreshToken, err
}

func (s *BoltStorage) DeleteRefreshToken(ctx context.Context, token string) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketRefreshTokens))
		return b.Delete([]byte(token))
	})
}

// Clients
func (s *BoltStorage) StoreClient(ctx context.Context, client *RegisteredClient) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		data, err := json.Marshal(client)
		if err != nil {
			return err
		}
		return b.Put([]byte(client.ClientID), data)
	})
}

func (s *BoltStorage) GetClient(ctx context.Context, clientID string) (*RegisteredClient, error) {
	var client *RegisteredClient
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		data := b.Get([]byte(clientID))
		if data == nil {
			return ErrClientNotFound
		}
		client = &RegisteredClient{}
		return json.Unmarshal(data, client)
	})
	return client, err
}

func (s *BoltStorage) DeleteClient(ctx context.Context, clientID string) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		return b.Delete([]byte(clientID))
	})
}

func (s *BoltStorage) ListClients(ctx context.Context) ([]*RegisteredClient, error) {
	var clients []*RegisteredClient
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		return b.ForEach(func(k, v []byte) error {
			var client RegisteredClient
			if err := json.Unmarshal(v, &client); err != nil {
				return err
			}
			clients = append(clients, &client)
			return nil
		})
	})
	return clients, err
}

// Users
func (s *BoltStorage) StoreUser(ctx context.Context, user *User) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsers))
		data, err := json.Marshal(user)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(user.ID), data); err != nil {
			return err
		}

		// Index by provider identity, once a login has recorded one.
		if user.Provider != "" && user.Issuer != "" && user.Subject != "" {
			key := identityKey(user.Provider, user.Issuer, user.Subject)
			if err := tx.Bucket([]byte(BucketUsersByIdentity)).Put(key, []byte(user.ID)); err != nil {
				return err
			}
		}

		// Index by GitHub login. Users of other providers have none (and bbolt refuses an
		// empty key).
		if user.GitHubLogin == "" {
			return nil
		}
		bGH := tx.Bucket([]byte(BucketUsersByGitHub))
		return bGH.Put([]byte(user.GitHubLogin), []byte(user.ID))
	})
}

// identityKey is the users_by_identity key. NUL cannot occur in a provider name or an issuer
// URL, so distinct triples cannot collide.
func identityKey(provider, issuer, subject string) []byte {
	return []byte(provider + "\x00" + issuer + "\x00" + subject)
}

func (s *BoltStorage) GetUserByIdentity(ctx context.Context, provider, issuer, subject string) (*User, error) {
	var user *User
	err := s.view(func(tx *bolt.Tx) error {
		userID := tx.Bucket([]byte(BucketUsersByIdentity)).Get(identityKey(provider, issuer, subject))
		if userID == nil {
			return ErrUserNotFound
		}
		data := tx.Bucket([]byte(BucketUsers)).Get(userID)
		if data == nil {
			return ErrUserNotFound
		}
		user = &User{}
		return json.Unmarshal(data, user)
	})
	return user, err
}

// forEachUser calls fn with every stored user, stopping at the first error. It is for startup
// checks, not for the request path.
func (s *BoltStorage) forEachUser(fn func(*User) error) error {
	return s.view(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketUsers)).ForEach(func(key, data []byte) error {
			var u User
			if err := json.Unmarshal(data, &u); err != nil {
				return fmt.Errorf("auth store %s: user record %q: %w", s.path, key, err)
			}
			return fn(&u)
		})
	})
}

func (s *BoltStorage) GetUser(ctx context.Context, userID string) (*User, error) {
	var user *User
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsers))
		data := b.Get([]byte(userID))
		if data == nil {
			return ErrUserNotFound
		}
		user = &User{}
		return json.Unmarshal(data, user)
	})
	return user, err
}

func (s *BoltStorage) GetUserByGitHubLogin(ctx context.Context, login string) (*User, error) {
	var user *User
	err := s.view(func(tx *bolt.Tx) error {
		bGH := tx.Bucket([]byte(BucketUsersByGitHub))
		userID := bGH.Get([]byte(login))
		if userID == nil {
			return ErrUserNotFound
		}

		b := tx.Bucket([]byte(BucketUsers))
		data := b.Get(userID)
		if data == nil {
			return ErrUserNotFound
		}
		user = &User{}
		return json.Unmarshal(data, user)
	})
	return user, err
}

// Sessions
func (s *BoltStorage) StoreSession(ctx context.Context, session *AuthSession) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketSessions))
		data, err := json.Marshal(session)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(session.SessionID), data); err != nil {
			return err
		}

		// Index by access token
		bToken := tx.Bucket([]byte(BucketSessionsByToken))
		return bToken.Put([]byte(session.AccessToken), []byte(session.SessionID))
	})
}

func (s *BoltStorage) GetSession(ctx context.Context, sessionID string) (*AuthSession, error) {
	var session *AuthSession
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketSessions))
		data := b.Get([]byte(sessionID))
		if data == nil {
			return ErrSessionNotFound
		}
		session = &AuthSession{}
		return json.Unmarshal(data, session)
	})
	return session, err
}

func (s *BoltStorage) GetSessionByAccessToken(ctx context.Context, token string) (*AuthSession, error) {
	var session *AuthSession
	err := s.view(func(tx *bolt.Tx) error {
		bToken := tx.Bucket([]byte(BucketSessionsByToken))
		sessionID := bToken.Get([]byte(token))
		if sessionID == nil {
			return ErrSessionNotFound
		}

		b := tx.Bucket([]byte(BucketSessions))
		data := b.Get(sessionID)
		if data == nil {
			return ErrSessionNotFound
		}
		session = &AuthSession{}
		return json.Unmarshal(data, session)
	})
	return session, err
}

func (s *BoltStorage) UpdateSessionLastUsed(ctx context.Context, sessionID string, lastUsed time.Time) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketSessions))
		data := b.Get([]byte(sessionID))
		if data == nil {
			return ErrSessionNotFound
		}

		var session AuthSession
		if err := json.Unmarshal(data, &session); err != nil {
			return err
		}

		session.LastUsedAt = lastUsed
		newData, err := json.Marshal(session)
		if err != nil {
			return err
		}
		return b.Put([]byte(sessionID), newData)
	})
}

func (s *BoltStorage) DeleteSession(ctx context.Context, sessionID string) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketSessions))
		data := b.Get([]byte(sessionID))
		if data != nil {
			var session AuthSession
			if err := json.Unmarshal(data, &session); err == nil {
				// Delete token index
				bToken := tx.Bucket([]byte(BucketSessionsByToken))
				bToken.Delete([]byte(session.AccessToken))
			}
		}
		return b.Delete([]byte(sessionID))
	})
}

// Pending Auth Requests
func (s *BoltStorage) StoreAuthRequest(ctx context.Context, req *PendingAuthRequest) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAuthRequests))
		data, err := json.Marshal(req)
		if err != nil {
			return err
		}
		return b.Put([]byte(req.ID), data)
	})
}

func (s *BoltStorage) GetAuthRequest(ctx context.Context, id string) (*PendingAuthRequest, error) {
	var req *PendingAuthRequest
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAuthRequests))
		data := b.Get([]byte(id))
		if data == nil {
			return fmt.Errorf("auth request not found")
		}
		req = &PendingAuthRequest{}
		return json.Unmarshal(data, req)
	})
	return req, err
}

func (s *BoltStorage) DeleteAuthRequest(ctx context.Context, id string) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAuthRequests))
		return b.Delete([]byte(id))
	})
}

// Consent

func (s *BoltStorage) StorePendingConsent(ctx context.Context, pc *PendingConsent) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPendingConsents))
		data, err := json.Marshal(pc)
		if err != nil {
			return err
		}
		return b.Put([]byte(pc.ID), data)
	})
}

// TakePendingConsent claims a pending consent in one transaction: it is returned and deleted
// only when it exists, has not expired, and bindingHash matches the one stored with it.
//
// A binding mismatch leaves the record in place. Otherwise anyone who learned a consent ID could
// burn it by posting with the wrong cookie, and the legitimate browser's approval would then
// fail. An expired record is deleted, since nothing can ever claim it.
func (s *BoltStorage) TakePendingConsent(ctx context.Context, id, bindingHash string) (*PendingConsent, error) {
	if id == "" {
		return nil, ErrConsentNotFound
	}

	var pc *PendingConsent
	expired := false
	err := s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPendingConsents))
		data := b.Get([]byte(id))
		if data == nil {
			return ErrConsentNotFound
		}

		var rec PendingConsent
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}

		// Returning an error from Update rolls the transaction back, so the expired record's
		// deletion is committed by returning nil and reporting expiry afterwards.
		if time.Now().After(rec.ExpiresAt) {
			expired = true
			return b.Delete([]byte(id))
		}
		if subtle.ConstantTimeCompare([]byte(rec.BindingHash), []byte(bindingHash)) != 1 {
			return ErrConsentBindingMismatch
		}
		if err := b.Delete([]byte(id)); err != nil {
			return err
		}
		pc = &rec
		return nil
	})
	if err != nil {
		return nil, err
	}
	if expired {
		return nil, ErrConsentExpired
	}
	return pc, nil
}

// consentKey is the consents bucket key for a user's grant to one client. The NUL separator
// cannot occur in either generated identifier, so distinct pairs cannot collide.
func consentKey(userID, clientID string) []byte {
	return []byte(userID + "\x00" + clientID)
}

func (s *BoltStorage) StoreConsent(ctx context.Context, userID, clientID string) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketConsents))
		data, err := json.Marshal(struct {
			GrantedAt time.Time `json:"granted_at"`
		}{GrantedAt: time.Now()})
		if err != nil {
			return err
		}
		return b.Put(consentKey(userID, clientID), data)
	})
}

func (s *BoltStorage) HasConsent(ctx context.Context, userID, clientID string) (bool, error) {
	granted := false
	err := s.view(func(tx *bolt.Tx) error {
		granted = tx.Bucket([]byte(BucketConsents)).Get(consentKey(userID, clientID)) != nil
		return nil
	})
	return granted, err
}
