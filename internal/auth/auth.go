package auth

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"tinyauth/internal/docker"
	"tinyauth/internal/ldap"
	"tinyauth/internal/types"
	"tinyauth/internal/utils"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/sessions"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
)

type Auth struct {
	Config        types.AuthConfig
	Docker        *docker.Docker
	LoginAttempts map[string]*types.LoginAttempt
	LoginMutex    sync.RWMutex
	Store         *sessions.CookieStore
	LDAP          *ldap.LDAP
}

func NewAuth(config types.AuthConfig, docker *docker.Docker, ldap *ldap.LDAP) *Auth {
	// Create cookie store
	store := sessions.NewCookieStore([]byte(config.HMACSecret), []byte(config.EncryptionSecret))

	// Configure cookie store
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   config.SessionExpiry,
		Secure:   config.CookieSecure,
		HttpOnly: true,
		Domain:   fmt.Sprintf(".%s", config.Domain),
	}

	return &Auth{
		Config:        config,
		Docker:        docker,
		LoginAttempts: make(map[string]*types.LoginAttempt),
		Store:         store,
		LDAP:          ldap,
	}
}

func (auth *Auth) GetSession(c *gin.Context) (*sessions.Session, error) {
	// Get session
	session, err := auth.Store.Get(c.Request, auth.Config.SessionCookieName)

	if err != nil {
		log.Warn().Err(err).Msg("Invalid session, clearing cookie and retrying")

		// Delete the session cookie if there is an error
		c.SetCookie(auth.Config.SessionCookieName, "", -1, "/", fmt.Sprintf(".%s", auth.Config.Domain), auth.Config.CookieSecure, true)

		// Try to get the session again
		session, err = auth.Store.Get(c.Request, auth.Config.SessionCookieName)

		if err != nil {
			// If we still can't get the session, log the error and return nil
			log.Error().Err(err).Msg("Failed to get session")
			return nil, err
		}
	}

	return session, nil
}

func (auth *Auth) SearchUser(username string) types.UserSearch {
	// Loop through users and return the user if the username matches
	log.Debug().Str("username", username).Msg("Searching for user")

	if auth.GetLocalUser(username).Username != "" {
		log.Debug().Str("username", username).Msg("Found local user")

		// If user found, return a user with the username and type "local"
		return types.UserSearch{
			Username: username,
			Type:     "local",
		}
	}

	// If no user found, check LDAP
	if auth.LDAP != nil {
		log.Debug().Str("username", username).Msg("Checking LDAP for user")

		userDN, err := auth.LDAP.Search(username)
		if err != nil {
			log.Warn().Err(err).Str("username", username).Msg("Failed to find user in LDAP")
			return types.UserSearch{}
		}

		// If user found in LDAP, return a user with the DN as username
		return types.UserSearch{
			Username: userDN,
			Type:     "ldap",
		}
	}

	return types.UserSearch{}
}

func (auth *Auth) VerifyUser(search types.UserSearch, password string) bool {
	// Authenticate the user based on the type
	switch search.Type {
	case "local":
		// Get local user
		user := auth.GetLocalUser(search.Username)

		// Check if password is correct
		return auth.CheckPassword(user, password)
	case "ldap":
		// If LDAP is configured, bind to the LDAP server with the user DN and password
		if auth.LDAP != nil {
			log.Debug().Str("username", search.Username).Msg("Binding to LDAP for user authentication")

			// Bind to the LDAP server
			err := auth.LDAP.Bind(search.Username, password)
			if err != nil {
				log.Warn().Err(err).Str("username", search.Username).Msg("Failed to bind to LDAP")
				return false
			}

			// If bind is successful, rebind with the LDAP bind user
			err = auth.LDAP.Bind(auth.LDAP.Config.BindDN, auth.LDAP.Config.BindPassword)
			if err != nil {
				log.Error().Err(err).Msg("Failed to rebind with service account after user authentication")
				// Consider closing the connection or creating a new one
				return false
			}

			log.Debug().Str("username", search.Username).Msg("LDAP authentication successful")

			// Return true if the bind was successful
			return true
		}
	default:
		log.Warn().Str("type", search.Type).Msg("Unknown user type for authentication")
		return false
	}

	// If no user found or authentication failed, return false
	log.Warn().Str("username", search.Username).Msg("User authentication failed")
	return false
}

func (auth *Auth) GetLocalUser(username string) types.User {
	// Loop through users and return the user if the username matches
	log.Debug().Str("username", username).Msg("Searching for local user")

	for _, user := range auth.Config.Users {
		if user.Username == username {
			return user
		}
	}

	// If no user found, return an empty user
	log.Warn().Str("username", username).Msg("Local user not found")
	return types.User{}
}

func (auth *Auth) CheckPassword(user types.User, password string) bool {
	// Compare the hashed password with the password provided
	return bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)) == nil
}

// IsAccountLocked checks if a username or IP is locked due to too many failed login attempts
func (auth *Auth) IsAccountLocked(identifier string) (bool, int) {
	auth.LoginMutex.RLock()
	defer auth.LoginMutex.RUnlock()

	// Return false if rate limiting is not configured
	if auth.Config.LoginMaxRetries <= 0 || auth.Config.LoginTimeout <= 0 {
		return false, 0
	}

	// Check if the identifier exists in the map
	attempt, exists := auth.LoginAttempts[identifier]
	if !exists {
		return false, 0
	}

	// If account is locked, check if lock time has expired
	if attempt.LockedUntil.After(time.Now()) {
		// Calculate remaining lockout time in seconds
		remaining := int(time.Until(attempt.LockedUntil).Seconds())
		return true, remaining
	}

	// Lock has expired
	return false, 0
}

// RecordLoginAttempt records a login attempt for rate limiting
func (auth *Auth) RecordLoginAttempt(identifier string, success bool) {
	// Skip if rate limiting is not configured
	if auth.Config.LoginMaxRetries <= 0 || auth.Config.LoginTimeout <= 0 {
		return
	}

	auth.LoginMutex.Lock()
	defer auth.LoginMutex.Unlock()

	// Get current attempt record or create a new one
	attempt, exists := auth.LoginAttempts[identifier]
	if !exists {
		attempt = &types.LoginAttempt{}
		auth.LoginAttempts[identifier] = attempt
	}

	// Update last attempt time
	attempt.LastAttempt = time.Now()

	// If successful login, reset failed attempts
	if success {
		attempt.FailedAttempts = 0
		attempt.LockedUntil = time.Time{} // Reset lock time
		return
	}

	// Increment failed attempts
	attempt.FailedAttempts++

	// If max retries reached, lock the account
	if attempt.FailedAttempts >= auth.Config.LoginMaxRetries {
		attempt.LockedUntil = time.Now().Add(time.Duration(auth.Config.LoginTimeout) * time.Second)
		log.Warn().Str("identifier", identifier).Int("timeout", auth.Config.LoginTimeout).Msg("Account locked due to too many failed login attempts")
	}
}

func (auth *Auth) EmailWhitelisted(emailSrc string) bool {
	return utils.CheckWhitelist(auth.Config.OauthWhitelist, emailSrc)
}

func (auth *Auth) CreateSessionCookie(c *gin.Context, data *types.SessionCookie) error {
	log.Debug().Msg("Creating session cookie")

	// Get session
	session, err := auth.GetSession(c)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get session")
		return err
	}

	log.Debug().Msg("Setting session cookie")

	// Calculate expiry
	var sessionExpiry int

	if data.TotpPending {
		sessionExpiry = 3600
	} else {
		sessionExpiry = auth.Config.SessionExpiry
	}

	// Set data
	session.Values["username"] = data.Username
	session.Values["name"] = data.Name
	session.Values["email"] = data.Email
	session.Values["provider"] = data.Provider
	session.Values["expiry"] = time.Now().Add(time.Duration(sessionExpiry) * time.Second).Unix()
	session.Values["totpPending"] = data.TotpPending
	session.Values["oauthGroups"] = data.OAuthGroups

	// Save session
	err = session.Save(c.Request, c.Writer)
	if err != nil {
		log.Error().Err(err).Msg("Failed to save session")
		return err
	}

	// Return nil
	return nil
}

func (auth *Auth) DeleteSessionCookie(c *gin.Context) error {
	log.Debug().Msg("Deleting session cookie")

	// Get session
	session, err := auth.GetSession(c)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get session")
		return err
	}

	// Delete all values in the session
	for key := range session.Values {
		delete(session.Values, key)
	}

	// Save session
	err = session.Save(c.Request, c.Writer)
	if err != nil {
		log.Error().Err(err).Msg("Failed to save session")
		return err
	}

	// Return nil
	return nil
}

func (auth *Auth) GetSessionCookie(c *gin.Context) (types.SessionCookie, error) {
	log.Debug().Msg("Getting session cookie")

	// Get session
	session, err := auth.GetSession(c)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get session")
		return types.SessionCookie{}, err
	}

	log.Debug().Msg("Got session")

	// Get data from session
	username, usernameOk := session.Values["username"].(string)
	email, emailOk := session.Values["email"].(string)
	name, nameOk := session.Values["name"].(string)
	provider, providerOK := session.Values["provider"].(string)
	expiry, expiryOk := session.Values["expiry"].(int64)
	totpPending, totpPendingOk := session.Values["totpPending"].(bool)
	oauthGroups, oauthGroupsOk := session.Values["oauthGroups"].(string)

	if !usernameOk || !providerOK || !expiryOk || !totpPendingOk || !emailOk || !nameOk || !oauthGroupsOk {
		log.Warn().Msg("Session cookie is invalid")

		// If any data is missing, delete the session cookie
		auth.DeleteSessionCookie(c)

		// Return empty cookie
		return types.SessionCookie{}, nil
	}

	// Check if the cookie has expired
	if time.Now().Unix() > expiry {
		log.Warn().Msg("Session cookie expired")

		// If it has, delete it
		auth.DeleteSessionCookie(c)

		// Return empty cookie
		return types.SessionCookie{}, nil
	}

	log.Debug().Str("username", username).Str("provider", provider).Int64("expiry", expiry).Bool("totpPending", totpPending).Str("name", name).Str("email", email).Str("oauthGroups", oauthGroups).Msg("Parsed cookie")

	// Return the cookie
	return types.SessionCookie{
		Username:    username,
		Name:        name,
		Email:       email,
		Provider:    provider,
		TotpPending: totpPending,
		OAuthGroups: oauthGroups,
	}, nil
}

func (auth *Auth) UserAuthConfigured() bool {
	// If there are users, return true
	return len(auth.Config.Users) > 0 || auth.LDAP != nil
}

func (auth *Auth) ResourceAllowed(c *gin.Context, context types.UserContext, labels types.Labels) bool {
	// Check if oauth is allowed
	if context.OAuth {
		log.Debug().Msg("Checking OAuth whitelist")
		return utils.CheckWhitelist(labels.OAuth.Whitelist, context.Email)
	}

	// Check users
	log.Debug().Msg("Checking users")

	return utils.CheckWhitelist(labels.Users, context.Username)
}

func (auth *Auth) OAuthGroup(c *gin.Context, context types.UserContext, labels types.Labels) bool {
	// Check if groups are required
	if labels.OAuth.Groups == "" {
		return true
	}

	// Check if we are using the generic oauth provider
	if context.Provider != "generic" {
		log.Debug().Msg("Not using generic provider, skipping group check")
		return true
	}

	// Split the groups by comma (no need to parse since they are from the API response)
	oauthGroups := strings.Split(context.OAuthGroups, ",")

	// For every group check if it is in the required groups
	for _, group := range oauthGroups {
		if utils.CheckWhitelist(labels.OAuth.Groups, group) {
			log.Debug().Str("group", group).Msg("Group is in required groups")
			return true
		}
	}

	// No groups matched
	log.Debug().Msg("No groups matched")

	// Return false
	return false
}

func (auth *Auth) AuthEnabled(c *gin.Context, labels types.Labels) (bool, error) {
	// Get headers
	uri := c.Request.Header.Get("X-Forwarded-Uri")

	// Check if the allowed label is empty
	if labels.Allowed == "" {
		// Auth enabled
		return true, nil
	}

	// Compile regex
	regex, err := regexp.Compile(labels.Allowed)

	// If there is an error, invalid regex, auth enabled
	if err != nil {
		log.Warn().Err(err).Msg("Invalid regex")
		return true, err
	}

	// Check if the uri matches the regex
	if regex.MatchString(uri) {
		// Auth disabled
		return false, nil
	}

	// Auth enabled
	return true, nil
}

func (auth *Auth) GetBasicAuth(c *gin.Context) *types.User {
	// Get the Authorization header
	username, password, ok := c.Request.BasicAuth()

	// If not ok, return an empty user
	if !ok {
		return nil
	}

	// Return the user
	return &types.User{
		Username: username,
		Password: password,
	}
}

func (auth *Auth) CheckIP(c *gin.Context, labels types.Labels) bool {
	// Get the IP address from the request
	ip := c.ClientIP()

	// Check if the IP is in block list
	for _, blocked := range labels.IP.Block {
		res, err := utils.FilterIP(blocked, ip)
		if err != nil {
			log.Warn().Err(err).Str("item", blocked).Msg("Invalid IP/CIDR in block list")
			continue
		}
		if res {
			log.Warn().Str("ip", ip).Str("item", blocked).Msg("IP is in blocked list, denying access")
			return false
		}
	}

	// For every IP in the allow list, check if the IP matches
	for _, allowed := range labels.IP.Allow {
		res, err := utils.FilterIP(allowed, ip)
		if err != nil {
			log.Warn().Err(err).Str("item", allowed).Msg("Invalid IP/CIDR in allow list")
			continue
		}
		if res {
			log.Debug().Str("ip", ip).Str("item", allowed).Msg("IP is in allowed list, allowing access")
			return true
		}
	}

	// If not in allowed range and allowed range is not empty, deny access
	if len(labels.IP.Allow) > 0 {
		log.Warn().Str("ip", ip).Msg("IP not in allow list, denying access")
		return false
	}

	log.Debug().Str("ip", ip).Msg("IP not in allow or block list, allowing by default")

	return true
}
