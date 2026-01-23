package auth

import (
	"context"
	"encoding/json"
	"fmt"
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

	// Close
	Close() error
}

// BoltStorage implements Storage using BoltDB
type BoltStorage struct {
	db *bolt.DB
}

// Bucket names
const (
	BucketAuthCodes     = "auth_codes"
	BucketAccessTokens  = "access_tokens"
	BucketRefreshTokens = "refresh_tokens"
	BucketClients       = "clients"
	BucketUsers         = "users"
	BucketUsersByGitHub = "users_by_github"
	BucketSessions      = "sessions"
	BucketSessionsByToken = "sessions_by_token"
	BucketAuthRequests  = "auth_requests"
)

// NewBoltStorage creates a new BoltDB storage
func NewBoltStorage(path string) (*BoltStorage, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("failed to open bolt db: %w", err)
	}

	// Create buckets
	err = db.Update(func(tx *bolt.Tx) error {
		buckets := []string{
			BucketAuthCodes, BucketAccessTokens, BucketRefreshTokens,
			BucketClients, BucketUsers, BucketUsersByGitHub,
			BucketSessions, BucketSessionsByToken, BucketAuthRequests,
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

	return &BoltStorage{db: db}, nil
}

// Close closes the database
func (s *BoltStorage) Close() error {
	return s.db.Close()
}

// Authorization Codes
func (s *BoltStorage) StoreAuthCode(ctx context.Context, code *AuthorizationCode) error {
	return s.db.Update(func(tx *bolt.Tx) error {
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
	err := s.db.View(func(tx *bolt.Tx) error {
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
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAuthCodes))
		return b.Delete([]byte(code))
	})
}

// Access Tokens
func (s *BoltStorage) StoreAccessToken(ctx context.Context, token *AccessToken) error {
	return s.db.Update(func(tx *bolt.Tx) error {
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
	err := s.db.View(func(tx *bolt.Tx) error {
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
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAccessTokens))
		return b.Delete([]byte(token))
	})
}

// Refresh Tokens
func (s *BoltStorage) StoreRefreshToken(ctx context.Context, token *RefreshToken) error {
	return s.db.Update(func(tx *bolt.Tx) error {
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
	err := s.db.View(func(tx *bolt.Tx) error {
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
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketRefreshTokens))
		return b.Delete([]byte(token))
	})
}

// Clients
func (s *BoltStorage) StoreClient(ctx context.Context, client *RegisteredClient) error {
	return s.db.Update(func(tx *bolt.Tx) error {
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
	err := s.db.View(func(tx *bolt.Tx) error {
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
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketClients))
		return b.Delete([]byte(clientID))
	})
}

func (s *BoltStorage) ListClients(ctx context.Context) ([]*RegisteredClient, error) {
	var clients []*RegisteredClient
	err := s.db.View(func(tx *bolt.Tx) error {
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
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsers))
		data, err := json.Marshal(user)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(user.ID), data); err != nil {
			return err
		}

		// Index by GitHub login
		bGH := tx.Bucket([]byte(BucketUsersByGitHub))
		return bGH.Put([]byte(user.GitHubLogin), []byte(user.ID))
	})
}

func (s *BoltStorage) GetUser(ctx context.Context, userID string) (*User, error) {
	var user *User
	err := s.db.View(func(tx *bolt.Tx) error {
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
	err := s.db.View(func(tx *bolt.Tx) error {
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
	return s.db.Update(func(tx *bolt.Tx) error {
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
	err := s.db.View(func(tx *bolt.Tx) error {
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
	err := s.db.View(func(tx *bolt.Tx) error {
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
	return s.db.Update(func(tx *bolt.Tx) error {
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
	return s.db.Update(func(tx *bolt.Tx) error {
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
	return s.db.Update(func(tx *bolt.Tx) error {
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
	err := s.db.View(func(tx *bolt.Tx) error {
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
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketAuthRequests))
		return b.Delete([]byte(id))
	})
}
