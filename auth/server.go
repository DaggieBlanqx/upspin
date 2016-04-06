/*
Package auth handles authentication of Upspin users.

Sample usage:

   authHandler := auth.NewHandler(&auth.Config{Lookup: context.User.Lookup})

   rawHandler := func(session auth.Session, w http.ResponseWriter, r *http.Request) {
   	user := session.User()
   	w.Write([]byte(fmt.Sprintf("Hello Authenticated user %v", user)))
   }
   http.HandleFunc("/hellowithauth", authHandler.Handle(rawHandler))
   // Configure TLS here if necessary ...
   ListenAndServeTLS(":443", nil)
*/
package auth

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"

	"upspin.googlesource.com/upspin.git/cache"
	"upspin.googlesource.com/upspin.git/cloud/netutil"
	"upspin.googlesource.com/upspin.git/key/keyloader"
	"upspin.googlesource.com/upspin.git/upspin"
)

// HandlerFunc is a type used by HTTP handler functions that want to use a Handler for authentication.
type HandlerFunc func(session Session, w http.ResponseWriter, r *http.Request)

// Handler is used by HTTP servers to authenticate Upspin users.
type Handler interface {
	// Handle is the chained handler function to register an authenticated handler. See example in package document.
	Handle(authHandlerFunc HandlerFunc) func(w http.ResponseWriter, r *http.Request)
}

// Config holds the configuration parameters for an instance of Handler.
type Config struct {
	// Lookup looks up user keys.
	Lookup func(userName upspin.UserName) ([]upspin.PublicKey, error)

	// AllowUnauthenticatedConnections allows unauthenticated connections, making it the caller's responsibility to check Handler.IsAuthenticated.
	AllowUnauthenticatedConnections bool
}

// Session contains information about the connection and the authenticated user, if any.
type Session interface {
	// User returns the user name present in the session. It may be empty. Note: user might not be authenticated.
	User() upspin.UserName

	// IsAuthenticated reports whether the user in the session is authenticated.
	IsAuthenticated() bool

	// Err reports the status of the session.
	Err() error
}

type sessionImpl struct {
	user      upspin.UserName
	isAuth    bool
	tlsUnique string // This must represent a tls.ConnectionState.TLSUnique
	err       error
}

var _ Session = (*sessionImpl)(nil)

// authHandler implements a Handler that ensures cryptography-grade authentication.
type authHandler struct {
	config       *Config
	sessionCache *cache.LRU // maps tlsUnique to AuthSession. Thread-safe.
}

var _ Handler = (*authHandler)(nil)

const (
	// maxSessions defines the maximum number of connections to remember before we re-auth them.
	// This also limits the number of parallel requests we can service, so do not set it to small numbers.
	maxSessions = 1000
)

// NewHandler creates a new instance of a Handler according to the given config, which must not be changed subsequently by the caller.
func NewHandler(config *Config) Handler {
	return &authHandler{
		config:       config,
		sessionCache: cache.NewLRU(maxSessions),
	}
}

// User implements Session.
func (s *sessionImpl) User() upspin.UserName {
	return s.user
}

// IsAuthenticated implements Session.
func (s *sessionImpl) IsAuthenticated() bool {
	return s.isAuth
}

// Err implements Session.
func (s *sessionImpl) Err() error {
	return s.err
}

func (ah *authHandler) setTLSUnique(session *sessionImpl, tlsUnique string) {
	if tlsUnique == "" {
		log.Printf("Invalid tlsUnique for user %q", session.user)
		return
	}
	ah.sessionCache.Add(tlsUnique, session)
}

func (ah *authHandler) getSessionByTLSUnique(tlsUnique string) *sessionImpl {
	session, ok := ah.sessionCache.Get(tlsUnique)
	if !ok {
		return nil
	}
	return session.(*sessionImpl)
}

func (ah *authHandler) doAuth(w http.ResponseWriter, r *http.Request) (*sessionImpl, error) {
	// The username must be in all communications, even after a TLS handshake.
	user := upspin.UserName(r.Header.Get(userNameHeader))
	if user == "" {
		return nil, errors.New("missing username in HTTP header")
	}
	// Is this a TLS connection?
	if r.TLS == nil {
		// Not a TLS connection, so nothing else to do here.
		return nil, errors.New("not a TLS secure connection")
	}
	// If we have a tlsUnique, let's use it.
	if len(r.TLS.TLSUnique) > 0 { // 1 is the min size allowed by TLS.
		session := ah.getSessionByTLSUnique(string(r.TLS.TLSUnique))
		if session != nil && session.user == user {
			// We have a user and it's now authenticated. Done.
			session.isAuth = true
			return session, nil
		}
	}
	// Let's authenticate from scratch, if we have enough info.
	if ah.config.Lookup == nil {
		return nil, errors.New("cannot authenticate: internal error: missing Lookup function")
	}
	keys, err := ah.config.Lookup(user)
	if err != nil {
		return nil, err
	}
	err = verifyRequest(user, keys, r)
	if err != nil {
		return nil, err
	}
	// Success! Create a new session and cache it if we have a TLSUnique.
	session := &sessionImpl{
		isAuth: true,
		user:   user,
	}
	// Cache TLS unique to speed up the process in further requests.
	if len(r.TLS.TLSUnique) > 0 {
		// 1 is the min size allowed by TLS.
		ah.setTLSUnique(session, string(r.TLS.TLSUnique))
	}
	return session, nil
}

func (ah *authHandler) Handle(authHandlerFunc HandlerFunc) func(w http.ResponseWriter, r *http.Request) {
	httpHandler := func(w http.ResponseWriter, r *http.Request) {
		// Perform authentication here, return the handler func used by the HTTP handler.
		var session *sessionImpl
		session, err := ah.doAuth(w, r)
		if err != nil {
			if !ah.config.AllowUnauthenticatedConnections {
				// Return an error to the client and do not call the underlying handler function.
				log.Printf("HTTPClient: auth error: %v", err)
				// To be precise, the user is only unauthenticated. But an unauthenticated user is also not authorized.
				w.WriteHeader(http.StatusUnauthorized)
				netutil.SendJSONError(w, "AuthHandler:", err)
				return
			}
			session = &sessionImpl{
				err: err,
			}
		}
		// session is guaranteed non-nil here.
		authHandlerFunc(session, w, r)
	}
	return httpHandler
}

// verifyRequest verifies whether named user has signed the HTTP request using one of the possible keys.
func verifyRequest(userName upspin.UserName, keys []upspin.PublicKey, req *http.Request) error {
	sig := req.Header.Get(signatureHeader)
	if sig == "" {
		return errors.New("no signature in header")
	}
	neededKeyType := req.Header.Get(signatureTypeHeader)
	if neededKeyType == "" {
		return errors.New("no signature type in header")
	}
	sigPieces := strings.Fields(sig)
	if len(sigPieces) != 2 {
		return fmt.Errorf("expected two integers in signature, got %d", len(sigPieces))
	}
	var rs, ss big.Int
	_, ok := rs.SetString(sigPieces[0], 10)
	if !ok {
		return errMissingSignature
	}
	_, ok = ss.SetString(sigPieces[1], 10)
	if !ok {
		return errMissingSignature
	}
	for _, k := range keys {
		ecdsaPubKey, keyType, err := keyloader.ParsePublicKey(k)
		if err != nil {
			return err
		}
		if keyType != neededKeyType {
			continue
		}
		hash := hashUserRequest(userName, req)
		if !ecdsa.Verify(ecdsaPubKey, hash, &rs, &ss) {
			return fmt.Errorf("signature verification failed for user %s", userName)
		}
		return nil
	}
	return fmt.Errorf("no keys found for user %s", userName)
}
