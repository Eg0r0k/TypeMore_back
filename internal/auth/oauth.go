package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"golang.org/x/oauth2/google"
)

// OAuth flow cookies. They are short-lived and must survive the top-level GET
// redirect back from the provider (SameSite=Lax allows that).
const (
	cookieOAuthState    = "tm_oauth_state"
	cookieOAuthVerifier = "tm_oauth_verifier"
	cookieOAuthLink     = "tm_oauth_link"
	oauthFlowTTL        = 10 * time.Minute
)

// oauthUser is the normalized profile the flow needs, regardless of provider.
type oauthUser struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// oauthProvider bundles a configured oauth2.Config with the provider-specific
// way to fetch the user's profile.
type oauthProvider struct {
	name     string
	cfg      *oauth2.Config
	userInfo func(ctx context.Context, client *http.Client) (oauthUser, error)
}

// buildProviders constructs the enabled OAuth providers from configuration. A
// provider with no client id/secret is skipped (disabled).
func (s *Service) buildProviders() map[string]*oauthProvider {
	out := make(map[string]*oauthProvider)
	for name, creds := range s.cfg.Providers {
		if creds.ClientID == "" || creds.ClientSecret == "" {
			continue
		}
		switch name {
		case ProviderGitHub:
			out[name] = s.buildGitHub(creds)
		case ProviderGoogle:
			out[name] = s.buildGoogle(creds)
		}
	}
	return out
}

// callbackURL is the redirect URI for a provider; it must match what is
// registered with the provider.
func (s *Service) callbackURL(provider string) string {
	return fmt.Sprintf("%s/api/v1/auth/oauth/%s/callback", s.cfg.OAuthRedirectBase, provider)
}

func (s *Service) buildGoogle(creds ProviderCredentials) *oauthProvider {
	endpoint := google.Endpoint
	if creds.AuthURL != "" {
		endpoint.AuthURL = creds.AuthURL
	}
	if creds.TokenURL != "" {
		endpoint.TokenURL = creds.TokenURL
	}
	userInfoURL := creds.UserInfoURL
	if userInfoURL == "" {
		userInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
	}
	cfg := &oauth2.Config{
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
		Endpoint:     endpoint,
		RedirectURL:  s.callbackURL(ProviderGoogle),
		Scopes:       []string{"openid", "email", "profile"},
	}
	return &oauthProvider{
		name: ProviderGoogle,
		cfg:  cfg,
		userInfo: func(ctx context.Context, client *http.Client) (oauthUser, error) {
			return fetchOIDCUserInfo(ctx, client, userInfoURL)
		},
	}
}

func (s *Service) buildGitHub(creds ProviderCredentials) *oauthProvider {
	endpoint := github.Endpoint
	if creds.AuthURL != "" {
		endpoint.AuthURL = creds.AuthURL
	}
	if creds.TokenURL != "" {
		endpoint.TokenURL = creds.TokenURL
	}
	userURL := creds.UserInfoURL
	if userURL == "" {
		userURL = "https://api.github.com/user"
	}
	emailsURL := creds.EmailsURL
	if emailsURL == "" {
		emailsURL = "https://api.github.com/user/emails"
	}
	cfg := &oauth2.Config{
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
		Endpoint:     endpoint,
		RedirectURL:  s.callbackURL(ProviderGitHub),
		Scopes:       []string{"read:user", "user:email"},
	}
	return &oauthProvider{
		name: ProviderGitHub,
		cfg:  cfg,
		userInfo: func(ctx context.Context, client *http.Client) (oauthUser, error) {
			return fetchGitHubUserInfo(ctx, client, userURL, emailsURL)
		},
	}
}

// handleOAuthStart begins the login/registration flow: it mints state + a PKCE
// verifier, stows them in short-lived cookies, and redirects the browser to the
// provider. It is a top-level GET navigation.
func (s *Service) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.oauth[chi.URLParam(r, "provider")]
	if !ok {
		s.writeError(w, r, apiErrProviderUnknown)
		return
	}
	authURL, err := s.beginOAuth(w, p, uuid.Nil)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleLinkStart begins an explicit provider-linking flow for the logged-in
// user. Unlike start it is a POST (CSRF-protected) and returns the authorize URL
// as JSON so the SPA can navigate to it.
func (s *Service) handleLinkStart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.oauth[chi.URLParam(r, "provider")]
	if !ok {
		s.writeError(w, r, apiErrProviderUnknown)
		return
	}
	user, ok := UserFrom(r.Context())
	if !ok {
		s.writeError(w, r, apiErrUnauthorized)
		return
	}
	authURL, err := s.beginOAuth(w, p, user.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"authorizeUrl": authURL})
}

// beginOAuth sets the state/verifier cookies (and the link cookie when linkUser
// is non-nil) and returns the provider authorize URL with a PKCE challenge.
func (s *Service) beginOAuth(w http.ResponseWriter, p *oauthProvider, linkUser uuid.UUID) (string, error) {
	state, _, err := newToken()
	if err != nil {
		return "", err
	}
	verifier := oauth2.GenerateVerifier()

	s.setFlowCookie(w, cookieOAuthState, state, oauthFlowTTL)
	s.setFlowCookie(w, cookieOAuthVerifier, verifier, oauthFlowTTL)
	if linkUser != uuid.Nil {
		s.setFlowCookie(w, cookieOAuthLink, linkUser.String(), oauthFlowTTL)
	}

	return p.cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier)), nil
}

// handleOAuthCallback completes the flow. It verifies the state cookie against
// the returned state (CSRF), exchanges the code with the PKCE verifier, fetches
// the profile, and then either logs in / creates an account or, in the link
// flow, attaches the identity to the current user. It always ends in a redirect
// back to the frontend carrying a status/error query parameter.
func (s *Service) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	p, ok := s.oauth[chi.URLParam(r, "provider")]
	if !ok {
		s.writeError(w, r, apiErrProviderUnknown)
		return
	}
	ctx := r.Context()

	// Read flow cookies, then clear them: this flow is one-shot.
	stateCookie, _ := r.Cookie(cookieOAuthState)
	verifierCookie, _ := r.Cookie(cookieOAuthVerifier)
	linkCookie, _ := r.Cookie(cookieOAuthLink)
	s.clearFlowCookies(w)

	state := r.URL.Query().Get("state")
	if stateCookie == nil || state == "" ||
		subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(state)) != 1 {
		s.redirectResult(w, r, "invalid_state")
		return
	}
	if verifierCookie == nil {
		s.redirectResult(w, r, "invalid_state")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.redirectResult(w, r, "oauth_denied")
		return
	}

	token, err := p.cfg.Exchange(ctx, code, oauth2.VerifierOption(verifierCookie.Value))
	if err != nil {
		s.log.ErrorContext(ctx, "oauth code exchange failed", "provider", p.name, "err", err)
		s.redirectResult(w, r, "oauth_exchange_failed")
		return
	}
	info, err := p.userInfo(ctx, p.cfg.Client(ctx, token))
	if err != nil {
		s.log.ErrorContext(ctx, "oauth userinfo failed", "provider", p.name, "err", err)
		s.redirectResult(w, r, "oauth_userinfo_failed")
		return
	}
	if info.Subject == "" {
		s.redirectResult(w, r, "oauth_userinfo_failed")
		return
	}
	info.Email = normalizeEmail(info.Email)

	if linkCookie != nil && linkCookie.Value != "" {
		s.completeLink(w, r, p, linkCookie.Value, info)
		return
	}
	s.completeLogin(w, r, p, info)
}

// completeLogin logs in an existing identity or creates a new account. It
// enforces the no-auto-link policy: a new identity whose verified email already
// belongs to another (verified) account is rejected with
// account_exists_use_linking.
func (s *Service) completeLogin(w http.ResponseWriter, r *http.Request, p *oauthProvider, info oauthUser) {
	ctx := r.Context()

	identity, err := s.store.IdentityByProviderSubject(ctx, p.name, info.Subject)
	switch {
	case err == nil:
		// Known identity: just log in.
		if serr := s.issueSession(ctx, w, identity.UserID); serr != nil {
			s.writeError(w, r, serr)
			return
		}
		s.redirectResult(w, r, "")
		return
	case !errors.Is(err, ErrNotFound):
		s.writeError(w, r, err)
		return
	}

	// New identity. No silent linking on matching email.
	if info.Email != "" && info.EmailVerified {
		if _, cerr := s.store.VerifiedIdentityByEmail(ctx, info.Email); cerr == nil {
			s.redirectResult(w, r, "account_exists_use_linking")
			return
		} else if !errors.Is(cerr, ErrNotFound) {
			s.writeError(w, r, cerr)
			return
		}
	}

	user, _, err := s.store.CreateOAuthAccount(ctx, OAuthAccountParams{
		DisplayName:   oauthDisplayName(info),
		Provider:      p.name,
		Subject:       info.Subject,
		Email:         info.Email,
		EmailVerified: info.EmailVerified,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.issueSession(ctx, w, user.ID); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.redirectResult(w, r, "")
}

// completeLink attaches the provider identity to the logged-in user. It refuses
// if the identity is already bound to some account.
func (s *Service) completeLink(w http.ResponseWriter, r *http.Request, p *oauthProvider, linkUserID string, info oauthUser) {
	ctx := r.Context()

	userID, err := uuid.Parse(linkUserID)
	if err != nil {
		s.redirectResult(w, r, "invalid_state")
		return
	}

	if _, err := s.store.IdentityByProviderSubject(ctx, p.name, info.Subject); err == nil {
		// Already linked (possibly to another account): do not move it.
		s.redirectResult(w, r, "provider_already_linked")
		return
	} else if !errors.Is(err, ErrNotFound) {
		s.writeError(w, r, err)
		return
	}

	if _, err := s.store.LinkIdentity(ctx, userID, IdentityParams{
		Provider:      p.name,
		Subject:       info.Subject,
		Email:         info.Email,
		EmailVerified: info.EmailVerified,
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.redirectResultParams(w, r, url.Values{"linked": {p.name}})
}

// oauthDisplayName picks a sensible display name from a profile.
func oauthDisplayName(info oauthUser) string {
	if info.Name != "" {
		return info.Name
	}
	if at := strings.IndexByte(info.Email, '@'); at > 0 {
		return info.Email[:at]
	}
	return "player"
}

// redirectResult redirects to the frontend OAuth landing page, with ?error=code
// on failure or ?status=ok on success.
func (s *Service) redirectResult(w http.ResponseWriter, r *http.Request, errCode string) {
	params := url.Values{}
	if errCode == "" {
		params.Set("status", "ok")
	} else {
		params.Set("error", errCode)
	}
	s.redirectResultParams(w, r, params)
}

func (s *Service) redirectResultParams(w http.ResponseWriter, r *http.Request, params url.Values) {
	http.Redirect(w, r, s.cfg.FrontendOrigin+"/auth/callback?"+params.Encode(), http.StatusFound)
}

// setFlowCookie sets a short-lived OAuth flow cookie.
func (s *Service) setFlowCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Domain:   s.cfg.CookieDomain,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  s.now().Add(ttl),
		MaxAge:   int(ttl.Seconds()),
	})
}

// clearFlowCookies expires all three flow cookies.
func (s *Service) clearFlowCookies(w http.ResponseWriter) {
	for _, name := range []string{cookieOAuthState, cookieOAuthVerifier, cookieOAuthLink} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			Domain:   s.cfg.CookieDomain,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
	}
}

// --- provider profile fetchers ---

// fetchOIDCUserInfo reads a standard OIDC userinfo document (Google).
func fetchOIDCUserInfo(ctx context.Context, client *http.Client, userInfoURL string) (oauthUser, error) {
	var body struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := getJSON(ctx, client, userInfoURL, &body); err != nil {
		return oauthUser{}, err
	}
	return oauthUser{
		Subject:       body.Sub,
		Email:         body.Email,
		EmailVerified: body.EmailVerified,
		Name:          body.Name,
	}, nil
}

// fetchGitHubUserInfo reads the GitHub user plus its verified-email list, since
// GitHub does not report email verification on the user endpoint.
func fetchGitHubUserInfo(ctx context.Context, client *http.Client, userURL, emailsURL string) (oauthUser, error) {
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := getJSON(ctx, client, userURL, &user); err != nil {
		return oauthUser{}, err
	}
	if user.ID == 0 {
		return oauthUser{}, errors.New("github: empty user id")
	}

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	// A missing/failing emails call is non-fatal: fall back to the user email.
	_ = getJSON(ctx, client, emailsURL, &emails)

	email, verified := user.Email, false
	for _, e := range emails {
		if e.Primary {
			email, verified = e.Email, e.Verified
			break
		}
	}

	name := user.Name
	if name == "" {
		name = user.Login
	}
	return oauthUser{
		Subject:       strconv.FormatInt(user.ID, 10),
		Email:         email,
		EmailVerified: verified,
		Name:          name,
	}, nil
}

// getJSON performs a GET (with a bounded timeout) and decodes a JSON body.
func getJSON(ctx context.Context, client *http.Client, endpoint string, dst any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("userinfo request to %s: status %d", endpoint, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}
