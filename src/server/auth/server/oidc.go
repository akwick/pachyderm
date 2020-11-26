package server

import (
	"crypto/rand"
	"encoding/base64"
	goerr "errors"
	"fmt"
	"net/http"
	"path"
	"time"

	"github.com/pachyderm/pachyderm/src/client/auth"
	"github.com/pachyderm/pachyderm/src/client/pkg/errors"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/watch"

	oidc "github.com/coreos/go-oidc"
	logrus "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
)

const threeMinutes = 3 * 60 // Passed to col.PutTTL (so value is in seconds)

// various oidc invalid argument errors. Use 'goerror' instead of internal
// 'errors' library b/c stack trace isn't useful
var (
	errNotConfigured = goerr.New("OIDC ID provider configuration not found")
	errAuthFailed    = goerr.New("authorization failed")
	errWatchFailed   = goerr.New("error watching OIDC state token (has it expired?)")
	errTokenDeleted  = goerr.New("error during authorization: OIDC state token expired")
)

// IDTokenClaims represents the set of claims in an OIDC ID token that we're concerned with
type IDTokenClaims struct {
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Groups        []string `json:"groups"`
}

// InternalOIDCProvider contains information about the configured OIDC ID
// provider, as well as auth information identifying Pachyderm in the ID
// provider (ClientID and ClientSecret), which Pachyderm needs to perform
// authorization with it.
type InternalOIDCProvider struct {
	// a points back to the owning auth/server.apiServer, currently just so that
	// InternalOIDCProvider can get an etcd client from it to read/write OIDC
	// state tokens to etcd during authorization
	a *apiServer

	// Prefix indicates the user-specified name given to this ID provider in the
	// Pachyderm auth config (i.e. taken from the IDP.Name field)
	Prefix string

	// Provider generates the ID provider login URL returned by GetOIDCLogin
	Provider *oidc.Provider

	// Issuer is the address of the OIDC ID provider (where we exchange
	// authorization codes for access tokens and get users' email addresses in
	// Authorize())
	Issuer string

	// IssuerOverride is the address of the OIDC provider, as seen from the
	// pachd pod. This should be set if Issuer doesn't resolve within the
	// cluster correctly, ex. if it's localhost for a minikube deployment.
	IssuerOverride string

	// ClientID is Pachyderm's identifier in the OIDC ID provider (generated by
	// the ID provider, and passed to Pachyderm by the cluster administrator via
	// SetConfig)
	ClientID string

	// ClientSecret is a shared secret with the ID provider, for doing the
	// auth-code -> access-token exchange.
	ClientSecret string

	// RedirectURI is used by GetOIDCLogin to generate a login URL that redirects
	// users back to Pachyderm (must be provided by the cluster administrator via
	// SetConfig, as only they know their network topology & Pachyderm's address
	// within it, and must be included in login URLs)
	RedirectURI string

	// Scopes is a list of scopes to request from the OIDC server. This is always
	// the standard "openid", "email" and "profile", plus any user-specified
	// scopes.
	Scopes []string

	// IgnoreEmailVerified indicates that we don't care about the `email_verified` claim.
	// This is usually bad but may be necessary for non-conformant providers.
	IgnoreEmailVerified bool

	// States is an etcd collection containing the state information associated
	// with every in-progress authentication session. /authorization-code/callback
	// places users' ID tokens in here when they authenticate successfully, and
	// Authenticate() retrieves those ID tokens, converts them to Pachyderm
	// tokens, and returns users' Pachyderm tokens back to them--all scoped to the
	// OIDC state token identifying the login session
	States col.Collection
}

// CryptoString returns a cryptographically random, URL safe string with length
// at least n
//
// TODO(msteffen): move away from UUIDv4 towards this (current implementation of
// UUIDv4 produces UUIDs via CSPRNG, but the UUIDv4 spec doesn't guarantee that
// behavior, and we shouldn't assume it going forward)
func CryptoString(n int) string {
	var numBytes int
	for n >= base64.RawURLEncoding.EncodedLen(numBytes) {
		numBytes++
	}
	b := make([]byte, numBytes)
	_, err := rand.Read(b)
	if err != nil {
		panic("could not generate cryptographically secure random string!")
	}

	return base64.RawURLEncoding.EncodeToString(b)
}

// half is a helper function used to log the first half of OIDC state tokens in
// logs.
//
// Per the description of handleOIDCLogin, we currently don't give error details
// to callers of Authenticate/handleOIDCCallback, to avoid accidentally leaking
// sensitive information to untrusted users, and instead log error information
// from pachd (where only kubernetes administrators can see it) with the state
// token inline. This way, legitimate users having trouble authenticating can
// show their state token to a cluster administrator and get error information
// from them. However, to avoid giving too much user information to Kubernetes
// cluster administrators, we don't want to log users' private credentials. So
// this function is used to log part of an OIDC state token--enough to associate
// error logs with a failing authentication flow, but not enough for a cluster
// administrator to impersonate a user.
func half(state string) string {
	return fmt.Sprintf("%s.../%d", state[:len(state)/2], len(state))
}

// NewOIDCSP creates a new InternalOIDCProvider object from the given parameters
func (a *apiServer) NewOIDCSP(name, issuer, issuerOverride, clientID, clientSecret, redirectURI string, additionalScopes []string, ignoreEmailVerified bool) (*InternalOIDCProvider, error) {
	// "openid" is a required scope for OpenID Connect flows.
	// "profile" and "email" are necessary for using the email as an identifier
	scopes := append([]string{oidc.ScopeOpenID, "profile", "email"}, additionalScopes...)
	o := &InternalOIDCProvider{
		a:                   a,
		Prefix:              name,
		Issuer:              issuer,
		IssuerOverride:      issuerOverride,
		ClientID:            clientID,
		ClientSecret:        clientSecret,
		RedirectURI:         redirectURI,
		Scopes:              scopes,
		IgnoreEmailVerified: ignoreEmailVerified,
		States: col.NewCollection(
			a.env.GetEtcdClient(),
			path.Join(oidcAuthnPrefix),
			nil,
			&auth.SessionInfo{},
			nil,
			nil,
		),
	}
	var err error
	ctx := context.Background()
	if issuerOverride != "" {
		client, err := RewriteClient(issuer, issuerOverride)
		if err != nil {
			return nil, err
		}
		ctx = oidc.ClientContext(ctx, client)
	}

	o.Provider, err = oidc.NewProvider(
		// Due to the implementation of go-oidc, this context is used for RPCs made
		// during future OIDC authentication sessions (for fetching keys, inside of
		// 'verifier.Verify(ctx, rawIDToken)'). Thus, it must not have a timeout.
		// We ideally should create a new context.WithCancel() and cancel that new
		// context if/when o.Provider is updated, but we don't have a convenient
		// place to put that cancel() call and the effect of this omission is
		// limited to in-flight authentication sessions at the moment that
		// o.Provider updated, so we're ignoring it.
		ctx,
		issuer)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// GetOIDCLoginURL uses the given state to generate a login URL for the OIDC provider object
func (o *InternalOIDCProvider) GetOIDCLoginURL(ctx context.Context) (string, string, error) {
	if o == nil {
		return "", "", errors.WithStack(errNotConfigured)
	}

	state := CryptoString(30)
	nonce := CryptoString(30)
	conf := oauth2.Config{
		ClientID:     o.ClientID,
		ClientSecret: o.ClientSecret,
		RedirectURL:  o.RedirectURI,
		Endpoint:     o.Provider.Endpoint(),
		Scopes:       o.Scopes,
	}

	if _, err := col.NewSTM(ctx, o.a.env.GetEtcdClient(), func(stm col.STM) error {
		return o.States.ReadWrite(stm).PutTTL(state, &auth.SessionInfo{
			Nonce: nonce, // read & verified by /authorization-code/callback
		}, threeMinutes)
	}); err != nil {
		return "", "", errors.Wrap(err, "could not create OIDC login session")
	}

	url := conf.AuthCodeURL(state,
		oauth2.SetAuthURLParam("response_type", "code"),
		oauth2.SetAuthURLParam("nonce", nonce))
	return url, state, nil
}

// OIDCStateToEmail takes the state token created for the OIDC session and
// uses it discover the email of the user who obtained the code (or verify that
// the code belongs to them). This is how Pachyderm currently implements OIDC
// authorization in a production cluster
func (o *InternalOIDCProvider) OIDCStateToEmail(ctx context.Context, state string) (email string, retErr error) {
	defer func() {
		logrus.Infof("converted OIDC state %q to email %q (or err: %v)",
			half(state), email, retErr)
	}()
	// reestablish watch in a loop, in case there's a watch error
	if err := backoff.RetryNotify(func() error {
		watcher, err := o.States.ReadOnly(ctx).WatchOne(state)
		if err != nil {
			logrus.Errorf("error watching OIDC state token %q during authorization: %v",
				half(state), err)
			return errors.WithStack(errWatchFailed)
		}
		defer watcher.Close()

		// lookup the token from the given state
		for e := range watcher.Watch() {
			if e.Type == watch.EventError {
				// reestablish watch (error not returned to user)
				return e.Err
			} else if e.Type == watch.EventDelete {
				return errors.WithStack(errTokenDeleted)
			}

			// see if there's an ID token attached to the OIDC state now
			var key string
			var si auth.SessionInfo
			if err := e.Unmarshal(&key, &si); err != nil {
				// retry watch (maybe a valid SessionInfo will appear later?)
				return errors.Wrapf(err, "error unmarshalling OIDC SessionInfo")
			}
			if si.ConversionErr {
				return errors.WithStack(errAuthFailed)
			} else if si.Email != "" {
				// Success
				email = si.Email
				return nil
			}
		}
		return nil
	}, backoff.New60sBackOff(), func(err error, d time.Duration) error {
		logrus.Errorf("error watching OIDC state token %q during authorization (retrying in %s): %v",
			half(state), d, err)
		if errors.Is(err, errWatchFailed) || errors.Is(err, errTokenDeleted) || errors.Is(err, errAuthFailed) {
			return err // don't retry, just return the error
		}
		return nil
	}); err != nil {
		return "", err
	}
	return email, nil
}

// handleOIDCExchange implements the /authorization-code/callback endpoint. In
// the success case, it converts the passed authorization code to an email
// address and associates the email address with the passed OIDC state token in
// the 'oidc-authns' collection.
//
// The error handling from this function is slightly delicate, as callers may
// have network access to Pachyderm, but may not have an OIDC account or any
// legitimate access to this cluster, so we want to avoid accidentally leaking
// operational details. In general:
// - This should not return an HTTP error with more information than pachctl
//   prints. Currently, pachctl only prints the OIDC state token presented by
//   the user and "Authorization failed" if the token exchange doesn't work
//   (indicated by SessionInfo.ConversionErr == true).
// - More information may be included in logs (which should only be accessible
//   Pachyderm administrators with kubectl access), and logs include enough
//   characters of any relevant OIDC state token to identify a particular login
//   flow. Thus if a user is legitimate, they can present their OIDC state token
//   (displayed by pachctl or their browser) to a cluster administrator, and the
//   cluster administrator can locate a detailed error in pachctl's logs.
//   Together they can resolve any authorization issues.
// - This should also not log any user credentials that would allow a
//   kubernetes cluster administrator to impersonate an individual user
//   undetected in Pachyderm or elsewhere. Where this logs OIDC state tokens, to
//   correlate authentication flows to error logs, it only logs the first half,
//   which is not enough to authenticate.
//
// If needed, Pachyderm cluster administrators can impersonate users by calling
// GetAuthToken(), but that call is logged and auditable.
func (a *apiServer) handleOIDCExchange(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	sp := a.getOIDCSP()
	if sp == nil {
		http.Error(w, errNotConfigured.Error(), http.StatusConflict)
		return
	}
	code := req.URL.Query()["code"][0]
	state := req.URL.Query()["state"][0]
	if state == "" || code == "" {
		http.Error(w,
			"invalid OIDC callback request: missing OIDC state token or authorization code",
			http.StatusBadRequest)
		return
	}

	// Verify the ID token, and if it's valid, add it to this state's SessionInfo
	// in etcd, so that any concurrent Authorize() calls can discover it and give
	// the caller a Pachyderm token.
	nonce, email, conversionErr := a.handleOIDCExchangeInternal(
		context.Background(), sp, code, state)
	_, etcdErr := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		var si auth.SessionInfo
		return sp.States.ReadWrite(stm).Update(state, &si, func() error {
			// nonce can only be checked inside etcd txn, but if nonces don't match
			// that's a non-retryable authentication error, so set conversionErr as
			// if handleOIDCExchangeInternal had errored and proceed
			if conversionErr == nil && nonce != si.Nonce {
				conversionErr = fmt.Errorf(
					"IDP nonce %v did not match Pachyderm's session nonce %v",
					nonce, si.Nonce)
			}
			if conversionErr == nil {
				si.Email = email
			} else {
				si.ConversionErr = true
			}
			return nil
		})
	})
	// Make exactly one call, to http.Error or http.Write, with either
	// conversionErr (non-retryable) or etcdErr (retryable) if either is set
	switch {
	case conversionErr != nil:
		// Don't give the user specific error information
		http.Error(w,
			fmt.Sprintf("authorization failed (OIDC state token: %q; Pachyderm "+
				"logs may contain more information)", half(state)),
			http.StatusUnauthorized)
	case etcdErr != nil:
		http.Error(w,
			fmt.Sprintf("temporary error during authorization (OIDC state token: "+
				"%q; Pachyderm logs may contain more information)", half(state)),
			http.StatusInternalServerError)
	default:
		// Success
		fmt.Fprintf(w, "You are now logged in. Go back to the terminal to use Pachyderm!")
	}
	// Wite more detailed error information into pachd's logs, if appropriate
	// (use two ifs here vs switch in case both are set)
	if conversionErr != nil {
		logrus.Errorf("could not convert authorization code (OIDC state: %q) %v",
			half(state), conversionErr)
	}
	if etcdErr != nil {
		logrus.Errorf("error storing OIDC authorization code in etcd (OIDC state: %q): %v",
			half(state), etcdErr)
	}
}

func (o *InternalOIDCProvider) validateIDToken(ctx context.Context, rawIDToken string) (*oidc.IDToken, *IDTokenClaims, error) {
	var verifier = o.Provider.Verifier(&oidc.Config{ClientID: o.ClientID})
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "could not verify token")
	}

	var claims IDTokenClaims
	if err := idToken.Claims(&claims); err != nil {
		return nil, nil, errors.Wrapf(err, "could not get claims")
	}

	if !claims.EmailVerified && !o.IgnoreEmailVerified {
		return nil, nil, errors.Wrapf(err, "email_verified claim was false")
	}
	return idToken, &claims, nil
}

func (o *InternalOIDCProvider) syncGroupMembership(ctx context.Context, claims *IDTokenClaims) error {
	groups := make([]string, len(claims.Groups))
	for i, g := range claims.Groups {
		groups[i] = fmt.Sprintf("group/%s:%s", o.Prefix, g)
	}
	// Sync group membership based on the groups claim, if any
	return o.a.setGroupsForUserInternal(ctx, claims.Email, groups)
}

// handleOIDCExchangeInternal is a convenience function for converting an
// authorization code into an access token. The caller (handleOIDCExchange) is
// responsible for storing any responses from this in etcd and sending an HTTP
// response to the user's browser.
func (a *apiServer) handleOIDCExchangeInternal(ctx context.Context, sp *InternalOIDCProvider, authCode, state string) (nonce, email string, retErr error) {
	// log request, but do not log auth code (short-lived, but senstive user authenticator)
	logrus.Infof("auth.OIDC.handleOIDCExchange { \"state\": %q }", half(state))
	defer func() {
		logrus.Infof("auth.OIDC.handleOIDCExchange { \"state\": %q, \"nonce\": %q, \"email\": %q }",
			half(state), nonce, email)
	}()
	conf := &oauth2.Config{
		ClientID:     sp.ClientID,
		ClientSecret: sp.ClientSecret,
		RedirectURL:  sp.RedirectURI,
		Scopes:       sp.Scopes,
		Endpoint:     sp.Provider.Endpoint(),
	}

	if sp.IssuerOverride != "" {
		client, err := RewriteClient(sp.Issuer, sp.IssuerOverride)
		if err != nil {
			return "", "", err
		}
		ctx = oidc.ClientContext(ctx, client)
	}

	// Use the authorization code that is pushed to the redirect
	tok, err := conf.Exchange(ctx, authCode)
	if err != nil {
		return "", "", errors.Wrapf(err, "failed to exchange code")
	}

	// Extract the ID Token from OAuth2 token.
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok {
		return "", "", errors.New("missing id token")
	}

	// Parse and verify ID Token payload.
	idToken, claims, err := sp.validateIDToken(ctx, rawIDToken)
	if err != nil {
		return "", "", errors.Wrapf(err, "could not verify token")
	}

	if err := sp.syncGroupMembership(ctx, claims); err != nil {
		return "", "", errors.Wrapf(err, "could not sync group membership")
	}

	return idToken.Nonce, claims.Email, nil
}

func (a *apiServer) serveOIDC() error {
	// serve OIDC handler to exchange the auth code
	http.HandleFunc("/authorization-code/callback", a.handleOIDCExchange)
	return http.ListenAndServe(fmt.Sprintf(":%v", a.env.OidcPort), nil)
}
