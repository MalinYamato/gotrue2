package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/supabase/gotrue/internal/conf"
	"github.com/supabase/gotrue/internal/models"
)

type TokenTestSuite struct {
	suite.Suite
	API    *API
	Config *conf.GlobalConfiguration

	RefreshToken *models.RefreshToken
	User         *models.User
}

func TestToken(t *testing.T) {
	os.Setenv("GOTRUE_RATE_LIMIT_HEADER", "My-Custom-Header")
	api, config, err := setupAPIForTest()
	require.NoError(t, err)

	ts := &TokenTestSuite{
		API:    api,
		Config: config,
	}
	defer api.db.Close()

	suite.Run(t, ts)
}

func (ts *TokenTestSuite) SetupTest() {
	ts.RefreshToken = nil
	models.TruncateAll(ts.API.db)

	// Create user & refresh token
	u, err := models.NewUser("12345678", "test@example.com", "password", ts.Config.JWT.Aud, nil)
	require.NoError(ts.T(), err, "Error creating test user model")
	t := time.Now()
	u.EmailConfirmedAt = &t
	u.BannedUntil = nil
	require.NoError(ts.T(), ts.API.db.Create(u), "Error saving new test user")

	ts.User = u
	ts.RefreshToken, err = models.GrantAuthenticatedUser(ts.API.db, u, models.GrantParams{})
	require.NoError(ts.T(), err, "Error creating refresh token")
}

func (ts *TokenTestSuite) TestFailedToSaveRefreshTokenResultCase() {
	var buffer bytes.Buffer

	// first refresh
	require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(map[string]interface{}{
		"refresh_token": ts.RefreshToken.Token,
	}))

	req := httptest.NewRequest(http.MethodPost, "http://localhost/token?grant_type=refresh_token", &buffer)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusOK, w.Code)

	var firstResult struct {
		RefreshToken string `json:"refresh_token"`
	}

	assert.NoError(ts.T(), json.NewDecoder(w.Result().Body).Decode(&firstResult))
	assert.NotEmpty(ts.T(), firstResult.RefreshToken)

	// pretend that the browser wasn't able to save the firstResult,
	// run again with the first refresh token
	buffer = bytes.Buffer{}

	// second refresh with the reused refresh token
	require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(map[string]interface{}{
		"refresh_token": ts.RefreshToken.Token,
	}))

	w = httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusOK, w.Code)

	var secondResult struct {
		RefreshToken string `json:"refresh_token"`
	}

	assert.NoError(ts.T(), json.NewDecoder(w.Result().Body).Decode(&secondResult))
	assert.NotEmpty(ts.T(), secondResult.RefreshToken)

	// new refresh token is not being issued but the active one from
	// the first refresh that failed to save is stored
	assert.Equal(ts.T(), firstResult.RefreshToken, secondResult.RefreshToken)
}

func (ts *TokenTestSuite) TestRateLimitTokenRefresh() {
	var buffer bytes.Buffer
	req := httptest.NewRequest(http.MethodPost, "http://localhost/token", &buffer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("My-Custom-Header", "1.2.3.4")

	// It rate limits after 30 requests
	for i := 0; i < 30; i++ {
		w := httptest.NewRecorder()
		ts.API.handler.ServeHTTP(w, req)
		assert.Equal(ts.T(), http.StatusBadRequest, w.Code)
	}
	w := httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusTooManyRequests, w.Code)

	// It ignores X-Forwarded-For by default
	req.Header.Set("X-Forwarded-For", "1.1.1.1")
	w = httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusTooManyRequests, w.Code)

	// It doesn't rate limit a new value for the limited header
	req = httptest.NewRequest(http.MethodPost, "http://localhost/token", &buffer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("My-Custom-Header", "5.6.7.8")
	w = httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusBadRequest, w.Code)
}

func (ts *TokenTestSuite) TestTokenPasswordGrantSuccess() {
	var buffer bytes.Buffer
	require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(map[string]interface{}{
		"email":    "test@example.com",
		"password": "password",
	}))

	req := httptest.NewRequest(http.MethodPost, "http://localhost/token?grant_type=password", &buffer)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusOK, w.Code)
}

func (ts *TokenTestSuite) TestTokenRefreshTokenGrantSuccess() {
	var buffer bytes.Buffer
	require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(map[string]interface{}{
		"refresh_token": ts.RefreshToken.Token,
	}))

	req := httptest.NewRequest(http.MethodPost, "http://localhost/token?grant_type=refresh_token", &buffer)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusOK, w.Code)
}

func (ts *TokenTestSuite) TestTokenPasswordGrantFailure() {
	u := ts.createBannedUser()

	var buffer bytes.Buffer
	require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(map[string]interface{}{
		"email":    u.GetEmail(),
		"password": "password",
	}))

	req := httptest.NewRequest(http.MethodPost, "http://localhost/token?grant_type=password", &buffer)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusBadRequest, w.Code)
}

func (ts *TokenTestSuite) TestTokenPKCEGrantFailure() {
	authCode := "1234563"
	codeVerifier := "4a9505b9-0857-42bb-ab3c-098b4d28ddc2"
	invalidAuthCode := authCode + "123"
	invalidVerifier := codeVerifier + "123"
	codeChallenge := sha256.Sum256([]byte(codeVerifier))
	challenge := base64.RawURLEncoding.EncodeToString(codeChallenge[:])
	flowState, err := models.NewFlowState("github", challenge, models.SHA256, models.OAuth)
	require.NoError(ts.T(), err)
	flowState.AuthCode = authCode
	require.NoError(ts.T(), ts.API.db.Create(flowState))
	cases := []struct {
		desc             string
		authCode         string
		codeVerifier     string
		grantType        string
		expectedHTTPCode int
	}{
		{
			desc:         "Invalid Authcode",
			authCode:     invalidAuthCode,
			codeVerifier: codeVerifier,
		},
		{
			desc:         "Invalid code verifier",
			authCode:     authCode,
			codeVerifier: invalidVerifier,
		},
		{
			desc:         "Invalid auth code and verifier",
			authCode:     invalidAuthCode,
			codeVerifier: invalidVerifier,
		},
	}
	for _, v := range cases {
		ts.Run(v.desc, func() {
			var buffer bytes.Buffer
			require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(map[string]interface{}{
				"code_verifier": v.codeVerifier,
				"auth_code":     v.authCode,
			}))
			req := httptest.NewRequest(http.MethodPost, "http://localhost/token?grant_type=pkce", &buffer)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			ts.API.handler.ServeHTTP(w, req)
			assert.Equal(ts.T(), http.StatusForbidden, w.Code)
		})
	}
}

func (ts *TokenTestSuite) TestTokenRefreshTokenGrantFailure() {
	_ = ts.createBannedUser()

	var buffer bytes.Buffer
	require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(map[string]interface{}{
		"refresh_token": ts.RefreshToken.Token,
	}))

	req := httptest.NewRequest(http.MethodPost, "http://localhost/token?grant_type=refresh_token", &buffer)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusBadRequest, w.Code)
}

func (ts *TokenTestSuite) TestTokenRefreshTokenRotation() {
	u, err := models.NewUser("", "foo@example.com", "password", ts.Config.JWT.Aud, nil)
	require.NoError(ts.T(), err, "Error creating test user model")
	t := time.Now()
	u.EmailConfirmedAt = &t
	require.NoError(ts.T(), ts.API.db.Create(u), "Error saving foo user")

	first, err := models.GrantAuthenticatedUser(ts.API.db, u, models.GrantParams{})
	require.NoError(ts.T(), err)
	second, err := models.GrantRefreshTokenSwap(&http.Request{}, ts.API.db, u, first)
	require.NoError(ts.T(), err)

	cases := []struct {
		desc                        string
		refreshTokenRotationEnabled bool
		reuseInterval               int
		refreshToken                string
		expectedCode                int
		expectedBody                map[string]interface{}
	}{
		{
			desc:                        "Valid refresh within reuse interval",
			refreshTokenRotationEnabled: true,
			reuseInterval:               30,
			refreshToken:                second.Token,
			expectedCode:                http.StatusOK,
			expectedBody: map[string]interface{}{
				"refresh_token": "some-new-refresh-token",
			},
		},
		{
			desc:                        "Invalid refresh outside reuse interval",
			refreshTokenRotationEnabled: true,
			reuseInterval:               0,
			refreshToken:                first.Token,
			expectedCode:                http.StatusBadRequest,
			expectedBody: map[string]interface{}{
				"error":             "invalid_grant",
				"error_description": "Invalid Refresh Token: Already Used",
			},
		},
		{
			desc:                        "Invalid refresh, revoke third token",
			refreshTokenRotationEnabled: true,
			reuseInterval:               0,
			refreshToken:                first.Token,
			expectedCode:                http.StatusBadRequest,
			expectedBody: map[string]interface{}{
				"error":             "invalid_grant",
				"error_description": "Invalid Refresh Token: Already Used",
			},
		},
	}

	for _, c := range cases {
		ts.Run(c.desc, func() {
			ts.Config.Security.RefreshTokenRotationEnabled = c.refreshTokenRotationEnabled
			ts.Config.Security.RefreshTokenReuseInterval = c.reuseInterval
			var buffer bytes.Buffer
			require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(map[string]interface{}{
				"refresh_token": c.refreshToken,
			}))
			req := httptest.NewRequest(http.MethodPost, "http://localhost/token?grant_type=refresh_token", &buffer)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			ts.API.handler.ServeHTTP(w, req)
			assert.Equal(ts.T(), c.expectedCode, w.Code)

			data := make(map[string]interface{})
			require.NoError(ts.T(), json.NewDecoder(w.Body).Decode(&data))
			for k, v := range c.expectedBody {
				if k == "refresh_token" {
					require.NotEmpty(ts.T(), v, data[k])
				} else {
					require.Equal(ts.T(), v, data[k])
				}
			}
		})
	}
}

func (ts *TokenTestSuite) createBannedUser() *models.User {
	u, err := models.NewUser("", "banned@example.com", "password", ts.Config.JWT.Aud, nil)
	require.NoError(ts.T(), err, "Error creating test user model")
	t := time.Now()
	u.EmailConfirmedAt = &t
	t = t.Add(24 * time.Hour)
	u.BannedUntil = &t
	require.NoError(ts.T(), ts.API.db.Create(u), "Error saving new test banned user")

	ts.RefreshToken, err = models.GrantAuthenticatedUser(ts.API.db, u, models.GrantParams{})
	require.NoError(ts.T(), err, "Error creating refresh token")

	return u
}

func (ts *TokenTestSuite) TestTokenRefreshWithExpiredSession() {
	var err error

	now := time.Now().UTC().Add(-1 * time.Second)

	ts.RefreshToken, err = models.GrantAuthenticatedUser(ts.API.db, ts.User, models.GrantParams{
		SessionNotAfter: &now,
	})
	require.NoError(ts.T(), err, "Error creating refresh token")

	var buffer bytes.Buffer
	require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(map[string]interface{}{
		"refresh_token": ts.RefreshToken.Token,
	}))

	req := httptest.NewRequest(http.MethodPost, "http://localhost/token?grant_type=refresh_token", &buffer)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusBadRequest, w.Code)
}

func (ts *TokenTestSuite) TestTokenRefreshWithUnexpiredSession() {
	var err error

	now := time.Now().UTC().Add(1 * time.Second)

	ts.RefreshToken, err = models.GrantAuthenticatedUser(ts.API.db, ts.User, models.GrantParams{
		SessionNotAfter: &now,
	})
	require.NoError(ts.T(), err, "Error creating refresh token")

	var buffer bytes.Buffer
	require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(map[string]interface{}{
		"refresh_token": ts.RefreshToken.Token,
	}))

	req := httptest.NewRequest(http.MethodPost, "http://localhost/token?grant_type=refresh_token", &buffer)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusOK, w.Code)
}

func (ts *TokenTestSuite) TestMagicLinkPKCESignIn() {
	var buffer bytes.Buffer
	// Send OTP
	codeVerifier := "4a9505b9-0857-42bb-ab3c-098b4d28ddc2"
	codeChallenge := sha256.Sum256([]byte(codeVerifier))
	challenge := base64.RawURLEncoding.EncodeToString(codeChallenge[:])

	req := httptest.NewRequest(http.MethodPost, "/otp", &buffer)
	req.Header.Set("Content-Type", "application/json")
	require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(OtpParams{
		Email:               "test@example.com",
		CreateUser:          true,
		CodeChallengeMethod: "s256",
		CodeChallenge:       challenge,
	}))
	req = httptest.NewRequest(http.MethodPost, "/otp", &buffer)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	require.Equal(ts.T(), http.StatusOK, w.Code)

	u, err := models.FindUserByEmailAndAudience(ts.API.db, "test@example.com", ts.Config.JWT.Aud)
	require.NoError(ts.T(), err)

	// Verify OTP
	requestUrl := fmt.Sprintf("http://localhost/verify?type=%v&token=%v", "magiclink", u.RecoveryToken)
	req = httptest.NewRequest(http.MethodGet, requestUrl, &buffer)
	req.Header.Set("Content-Type", "application/json")

	w = httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	assert.Equal(ts.T(), http.StatusSeeOther, w.Code)
	rURL, _ := w.Result().Location()

	u, err = models.FindUserByEmailAndAudience(ts.API.db, "test@example.com", ts.Config.JWT.Aud)
	require.NoError(ts.T(), err)
	assert.True(ts.T(), u.IsConfirmed())

	f, err := url.ParseQuery(rURL.RawQuery)
	require.NoError(ts.T(), err)
	authCode := f.Get("code")
	assert.NotEmpty(ts.T(), authCode)
	// Extract token and sign in
	require.NoError(ts.T(), json.NewEncoder(&buffer).Encode(map[string]interface{}{
		"code_verifier": codeVerifier,
		"auth_code":     authCode,
	}))
	req = httptest.NewRequest(http.MethodPost, "http://localhost/token?grant_type=pkce", &buffer)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	ts.API.handler.ServeHTTP(w, req)
	require.Equal(ts.T(), http.StatusOK, w.Code)
	verifyResp := &AccessTokenResponse{}
	require.NoError(ts.T(), json.NewDecoder(w.Body).Decode(&verifyResp))
	require.NotEmpty(ts.T(), verifyResp.Token)

}
