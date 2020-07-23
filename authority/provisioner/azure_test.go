package provisioner

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/smallstep/assert"
	"github.com/smallstep/certificates/errs"
	"github.com/smallstep/cli/jose"
)

func TestAzure_Getters(t *testing.T) {
	p, err := generateAzure()
	assert.FatalError(t, err)
	if got := p.GetID(); got != p.TenantID {
		t.Errorf("Azure.GetID() = %v, want %v", got, p.TenantID)
	}
	if got := p.GetName(); got != p.Name {
		t.Errorf("Azure.GetName() = %v, want %v", got, p.Name)
	}
	if got := p.GetType(); got != TypeAzure {
		t.Errorf("Azure.GetType() = %v, want %v", got, TypeAzure)
	}
	kid, key, ok := p.GetEncryptedKey()
	if kid != "" || key != "" || ok == true {
		t.Errorf("Azure.GetEncryptedKey() = (%v, %v, %v), want (%v, %v, %v)",
			kid, key, ok, "", "", false)
	}
}

func TestAzure_GetTokenID(t *testing.T) {
	p1, srv, err := generateAzureWithServer()
	assert.FatalError(t, err)
	defer srv.Close()

	p2, err := generateAzure()
	assert.FatalError(t, err)
	p2.TenantID = p1.TenantID
	p2.config = p1.config
	p2.oidcConfig = p1.oidcConfig
	p2.keyStore = p1.keyStore
	p2.DisableTrustOnFirstUse = true

	t1, err := p1.GetIdentityToken("subject", "caURL")
	assert.FatalError(t, err)
	t2, err := p2.GetIdentityToken("subject", "caURL")
	assert.FatalError(t, err)

	sum := sha256.Sum256([]byte("/subscriptions/subscriptionID/resourceGroups/resourceGroup/providers/Microsoft.Compute/virtualMachines/virtualMachine"))
	w1 := strings.ToLower(hex.EncodeToString(sum[:]))

	type args struct {
		token string
	}
	tests := []struct {
		name    string
		azure   *Azure
		args    args
		want    string
		wantErr bool
	}{
		{"ok", p1, args{t1}, w1, false},
		{"ok no TOFU", p2, args{t2}, "the-jti", false},
		{"fail token", p1, args{"bad-token"}, "", true},
		{"fail claims", p1, args{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.ey.fooo"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.azure.GetTokenID(tt.args.token)
			if (err != nil) != tt.wantErr {
				t.Errorf("Azure.GetTokenID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Azure.GetTokenID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAzure_GetIdentityToken(t *testing.T) {
	p1, err := generateAzure()
	assert.FatalError(t, err)

	t1, err := generateAzureToken("subject", p1.oidcConfig.Issuer, azureDefaultAudience,
		p1.TenantID, "subscriptionID", "resourceGroup", "virtualMachine",
		time.Now(), &p1.keyStore.keySet.Keys[0])
	assert.FatalError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad-request":
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		case "/bad-json":
			w.Write([]byte(t1))
		default:
			w.Header().Add("Content-Type", "application/json")
			w.Write([]byte(fmt.Sprintf(`{"access_token":"%s"}`, t1)))
		}
	}))
	defer srv.Close()

	type args struct {
		subject string
		caURL   string
	}
	tests := []struct {
		name             string
		azure            *Azure
		args             args
		identityTokenURL string
		want             string
		wantErr          bool
	}{
		{"ok", p1, args{"subject", "caURL"}, srv.URL, t1, false},
		{"fail request", p1, args{"subject", "caURL"}, srv.URL + "/bad-request", "", true},
		{"fail unmarshal", p1, args{"subject", "caURL"}, srv.URL + "/bad-json", "", true},
		{"fail url", p1, args{"subject", "caURL"}, "://ca.smallstep.com", "", true},
		{"fail connect", p1, args{"subject", "caURL"}, "foobarzar", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.azure.config.identityTokenURL = tt.identityTokenURL
			got, err := tt.azure.GetIdentityToken(tt.args.subject, tt.args.caURL)
			if (err != nil) != tt.wantErr {
				t.Errorf("Azure.GetIdentityToken() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Azure.GetIdentityToken() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAzure_Init(t *testing.T) {
	p1, srv, err := generateAzureWithServer()
	assert.FatalError(t, err)
	defer srv.Close()

	config := Config{
		Claims: globalProvisionerClaims,
	}
	badClaims := &Claims{
		DefaultTLSDur: &Duration{0},
	}

	badDiscoveryURL := &azureConfig{
		oidcDiscoveryURL: srv.URL + "/error",
		identityTokenURL: p1.config.identityTokenURL,
	}
	badJWKURL := &azureConfig{
		oidcDiscoveryURL: srv.URL + "/openid-configuration-fail-jwk",
		identityTokenURL: p1.config.identityTokenURL,
	}
	badAzureConfig := &azureConfig{
		oidcDiscoveryURL: srv.URL + "/openid-configuration-no-issuer",
		identityTokenURL: p1.config.identityTokenURL,
	}

	type fields struct {
		Type     string
		Name     string
		TenantID string
		Claims   *Claims
		config   *azureConfig
	}
	type args struct {
		config Config
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{"ok", fields{p1.Type, p1.Name, p1.TenantID, nil, p1.config}, args{config}, false},
		{"ok with config", fields{p1.Type, p1.Name, p1.TenantID, nil, p1.config}, args{config}, false},
		{"fail type", fields{"", p1.Name, p1.TenantID, nil, p1.config}, args{config}, true},
		{"fail name", fields{p1.Type, "", p1.TenantID, nil, p1.config}, args{config}, true},
		{"fail tenant id", fields{p1.Type, p1.Name, "", nil, p1.config}, args{config}, true},
		{"fail claims", fields{p1.Type, p1.Name, p1.TenantID, badClaims, p1.config}, args{config}, true},
		{"fail discovery URL", fields{p1.Type, p1.Name, p1.TenantID, nil, badDiscoveryURL}, args{config}, true},
		{"fail JWK URL", fields{p1.Type, p1.Name, p1.TenantID, nil, badJWKURL}, args{config}, true},
		{"fail config Validate", fields{p1.Type, p1.Name, p1.TenantID, nil, badAzureConfig}, args{config}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Azure{
				Type:     tt.fields.Type,
				Name:     tt.fields.Name,
				TenantID: tt.fields.TenantID,
				Claims:   tt.fields.Claims,
				config:   tt.fields.config,
			}
			if err := p.Init(tt.args.config); (err != nil) != tt.wantErr {
				t.Errorf("Azure.Init() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAzure_authorizeToken(t *testing.T) {
	type test struct {
		p     *Azure
		token string
		err   error
		code  int
	}
	tests := map[string]func(*testing.T) test{
		"fail/bad-token": func(t *testing.T) test {
			p, err := generateAzure()
			assert.FatalError(t, err)
			return test{
				p:     p,
				token: "foo",
				code:  http.StatusUnauthorized,
				err:   errors.New("azure.authorizeToken; error parsing azure token"),
			}
		},
		"fail/cannot-validate-sig": func(t *testing.T) test {
			p, srv, err := generateAzureWithServer()
			assert.FatalError(t, err)
			defer srv.Close()
			jwk, err := jose.GenerateJWK("EC", "P-256", "ES256", "sig", "", 0)
			assert.FatalError(t, err)
			tok, err := generateAzureToken("subject", p.oidcConfig.Issuer, azureDefaultAudience,
				p.TenantID, "subscriptionID", "resourceGroup", "virtualMachine",
				time.Now(), jwk)
			assert.FatalError(t, err)
			return test{
				p:     p,
				token: tok,
				code:  http.StatusUnauthorized,
				err:   errors.New("azure.authorizeToken; cannot validate azure token"),
			}
		},
		"fail/invalid-token-issuer": func(t *testing.T) test {
			p, srv, err := generateAzureWithServer()
			assert.FatalError(t, err)
			defer srv.Close()
			tok, err := generateAzureToken("subject", "bad-issuer", azureDefaultAudience,
				p.TenantID, "subscriptionID", "resourceGroup", "virtualMachine",
				time.Now(), &p.keyStore.keySet.Keys[0])
			assert.FatalError(t, err)
			return test{
				p:     p,
				token: tok,
				code:  http.StatusUnauthorized,
				err:   errors.New("azure.authorizeToken; failed to validate azure token payload"),
			}
		},
		"fail/invalid-tenant-id": func(t *testing.T) test {
			p, srv, err := generateAzureWithServer()
			assert.FatalError(t, err)
			defer srv.Close()
			tok, err := generateAzureToken("subject", p.oidcConfig.Issuer, azureDefaultAudience,
				"foo", "subscriptionID", "resourceGroup", "virtualMachine",
				time.Now(), &p.keyStore.keySet.Keys[0])
			assert.FatalError(t, err)
			return test{
				p:     p,
				token: tok,
				code:  http.StatusUnauthorized,
				err:   errors.New("azure.authorizeToken; azure token validation failed - invalid tenant id claim (tid)"),
			}
		},
		"fail/invalid-xms-mir-id": func(t *testing.T) test {
			p, srv, err := generateAzureWithServer()
			assert.FatalError(t, err)
			defer srv.Close()
			jwk := &p.keyStore.keySet.Keys[0]
			sig, err := jose.NewSigner(
				jose.SigningKey{Algorithm: jose.ES256, Key: jwk.Key},
				new(jose.SignerOptions).WithType("JWT").WithHeader("kid", jwk.KeyID),
			)
			assert.FatalError(t, err)

			now := time.Now()
			claims := azurePayload{
				Claims: jose.Claims{
					Subject:   "subject",
					Issuer:    p.oidcConfig.Issuer,
					IssuedAt:  jose.NewNumericDate(now),
					NotBefore: jose.NewNumericDate(now),
					Expiry:    jose.NewNumericDate(now.Add(5 * time.Minute)),
					Audience:  []string{azureDefaultAudience},
					ID:        "the-jti",
				},
				AppID:            "the-appid",
				AppIDAcr:         "the-appidacr",
				IdentityProvider: "the-idp",
				ObjectID:         "the-oid",
				TenantID:         p.TenantID,
				Version:          "the-version",
				XMSMirID:         "foo",
			}
			tok, err := jose.Signed(sig).Claims(claims).CompactSerialize()
			assert.FatalError(t, err)
			return test{
				p:     p,
				token: tok,
				code:  http.StatusUnauthorized,
				err:   errors.New("azure.authorizeToken; error parsing xms_mirid claim - foo"),
			}
		},
		"ok": func(t *testing.T) test {
			p, srv, err := generateAzureWithServer()
			assert.FatalError(t, err)
			defer srv.Close()
			tok, err := generateAzureToken("subject", p.oidcConfig.Issuer, azureDefaultAudience,
				p.TenantID, "subscriptionID", "resourceGroup", "virtualMachine",
				time.Now(), &p.keyStore.keySet.Keys[0])
			assert.FatalError(t, err)
			return test{
				p:     p,
				token: tok,
			}
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tc := tt(t)
			if claims, name, group, err := tc.p.authorizeToken(tc.token); err != nil {
				if assert.NotNil(t, tc.err) {
					sc, ok := err.(errs.StatusCoder)
					assert.Fatal(t, ok, "error does not implement StatusCoder interface")
					assert.Equals(t, sc.StatusCode(), tc.code)
					assert.HasPrefix(t, err.Error(), tc.err.Error())
				}
			} else {
				if assert.Nil(t, tc.err) {
					assert.Equals(t, claims.Subject, "subject")
					assert.Equals(t, claims.Issuer, tc.p.oidcConfig.Issuer)
					assert.Equals(t, claims.Audience[0], azureDefaultAudience)

					assert.Equals(t, name, "virtualMachine")
					assert.Equals(t, group, "resourceGroup")
				}
			}
		})
	}
}

func TestAzure_AuthorizeSign(t *testing.T) {
	p1, srv, err := generateAzureWithServer()
	assert.FatalError(t, err)
	defer srv.Close()

	p2, err := generateAzure()
	assert.FatalError(t, err)
	p2.TenantID = p1.TenantID
	p2.ResourceGroups = []string{"resourceGroup"}
	p2.config = p1.config
	p2.oidcConfig = p1.oidcConfig
	p2.keyStore = p1.keyStore
	p2.DisableCustomSANs = true

	p3, err := generateAzure()
	assert.FatalError(t, err)
	p3.config = p1.config
	p3.oidcConfig = p1.oidcConfig
	p3.keyStore = p1.keyStore

	p4, err := generateAzure()
	assert.FatalError(t, err)
	p4.TenantID = p1.TenantID
	p4.ResourceGroups = []string{"foobarzar"}
	p4.config = p1.config
	p4.oidcConfig = p1.oidcConfig
	p4.keyStore = p1.keyStore

	badKey, err := generateJSONWebKey()
	assert.FatalError(t, err)

	t1, err := p1.GetIdentityToken("subject", "caURL")
	assert.FatalError(t, err)
	t2, err := p2.GetIdentityToken("subject", "caURL")
	assert.FatalError(t, err)
	t3, err := p3.GetIdentityToken("subject", "caURL")
	assert.FatalError(t, err)
	t4, err := p4.GetIdentityToken("subject", "caURL")
	assert.FatalError(t, err)

	t11, err := generateAzureToken("subject", p1.oidcConfig.Issuer, azureDefaultAudience,
		p1.TenantID, "subscriptionID", "resourceGroup", "virtualMachine",
		time.Now(), &p1.keyStore.keySet.Keys[0])
	assert.FatalError(t, err)

	failIssuer, err := generateAzureToken("subject", "bad-issuer", azureDefaultAudience,
		p1.TenantID, "subscriptionID", "resourceGroup", "virtualMachine",
		time.Now(), &p1.keyStore.keySet.Keys[0])
	assert.FatalError(t, err)
	failAudience, err := generateAzureToken("subject", p1.oidcConfig.Issuer, "bad-audience",
		p1.TenantID, "subscriptionID", "resourceGroup", "virtualMachine",
		time.Now(), &p1.keyStore.keySet.Keys[0])
	assert.FatalError(t, err)
	failExp, err := generateAzureToken("subject", p1.oidcConfig.Issuer, azureDefaultAudience,
		p1.TenantID, "subscriptionID", "resourceGroup", "virtualMachine",
		time.Now().Add(-360*time.Second), &p1.keyStore.keySet.Keys[0])
	assert.FatalError(t, err)
	failNbf, err := generateAzureToken("subject", p1.oidcConfig.Issuer, azureDefaultAudience,
		p1.TenantID, "subscriptionID", "resourceGroup", "virtualMachine",
		time.Now().Add(360*time.Second), &p1.keyStore.keySet.Keys[0])
	assert.FatalError(t, err)
	failKey, err := generateAzureToken("subject", p1.oidcConfig.Issuer, azureDefaultAudience,
		p1.TenantID, "subscriptionID", "resourceGroup", "virtualMachine",
		time.Now(), badKey)
	assert.FatalError(t, err)

	type args struct {
		token string
	}
	tests := []struct {
		name    string
		azure   *Azure
		args    args
		wantLen int
		code    int
		wantErr bool
	}{
		{"ok", p1, args{t1}, 4, http.StatusOK, false},
		{"ok", p2, args{t2}, 9, http.StatusOK, false},
		{"ok", p1, args{t11}, 4, http.StatusOK, false},
		{"fail tenant", p3, args{t3}, 0, http.StatusUnauthorized, true},
		{"fail resource group", p4, args{t4}, 0, http.StatusUnauthorized, true},
		{"fail token", p1, args{"token"}, 0, http.StatusUnauthorized, true},
		{"fail issuer", p1, args{failIssuer}, 0, http.StatusUnauthorized, true},
		{"fail audience", p1, args{failAudience}, 0, http.StatusUnauthorized, true},
		{"fail exp", p1, args{failExp}, 0, http.StatusUnauthorized, true},
		{"fail nbf", p1, args{failNbf}, 0, http.StatusUnauthorized, true},
		{"fail key", p1, args{failKey}, 0, http.StatusUnauthorized, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := NewContextWithMethod(context.Background(), SignMethod)
			got, err := tt.azure.AuthorizeSign(ctx, tt.args.token)
			if (err != nil) != tt.wantErr {
				t.Errorf("Azure.AuthorizeSign() error = %v, wantErr %v", err, tt.wantErr)
				return
			} else if err != nil {
				sc, ok := err.(errs.StatusCoder)
				assert.Fatal(t, ok, "error does not implement StatusCoder interface")
				assert.Equals(t, sc.StatusCode(), tt.code)
			} else {
				assert.Len(t, tt.wantLen, got)
				for _, o := range got {
					switch v := o.(type) {
					case *provisionerExtensionOption:
						assert.Equals(t, v.Type, int(TypeAzure))
						assert.Equals(t, v.Name, tt.azure.GetName())
						assert.Equals(t, v.CredentialID, tt.azure.TenantID)
						assert.Len(t, 0, v.KeyValuePairs)
					case profileDefaultDuration:
						assert.Equals(t, time.Duration(v), tt.azure.claimer.DefaultTLSCertDuration())
					case commonNameValidator:
						assert.Equals(t, string(v), "virtualMachine")
					case defaultPublicKeyValidator:
					case *validityValidator:
						assert.Equals(t, v.min, tt.azure.claimer.MinTLSCertDuration())
						assert.Equals(t, v.max, tt.azure.claimer.MaxTLSCertDuration())
					case ipAddressesValidator:
						assert.Equals(t, v, nil)
					case emailAddressesValidator:
						assert.Equals(t, v, nil)
					case urisValidator:
						assert.Equals(t, v, nil)
					case dnsNamesValidator:
						assert.Equals(t, []string(v), []string{"virtualMachine"})
					default:
						assert.FatalError(t, errors.Errorf("unexpected sign option of type %T", v))
					}
				}
			}
		})
	}
}

func TestAzure_AuthorizeRenew(t *testing.T) {
	p1, err := generateAzure()
	assert.FatalError(t, err)
	p2, err := generateAzure()
	assert.FatalError(t, err)

	// disable renewal
	disable := true
	p2.Claims = &Claims{DisableRenewal: &disable}
	p2.claimer, err = NewClaimer(p2.Claims, globalProvisionerClaims)
	assert.FatalError(t, err)

	type args struct {
		cert *x509.Certificate
	}
	tests := []struct {
		name    string
		azure   *Azure
		args    args
		code    int
		wantErr bool
	}{
		{"ok", p1, args{nil}, http.StatusOK, false},
		{"fail/renew-disabled", p2, args{nil}, http.StatusUnauthorized, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.azure.AuthorizeRenew(context.Background(), tt.args.cert); (err != nil) != tt.wantErr {
				t.Errorf("Azure.AuthorizeRenew() error = %v, wantErr %v", err, tt.wantErr)
			} else if err != nil {
				sc, ok := err.(errs.StatusCoder)
				assert.Fatal(t, ok, "error does not implement StatusCoder interface")
				assert.Equals(t, sc.StatusCode(), tt.code)
			}
		})
	}
}

func TestAzure_AuthorizeSSHSign(t *testing.T) {
	tm, fn := mockNow()
	defer fn()

	p1, srv, err := generateAzureWithServer()
	assert.FatalError(t, err)
	p1.DisableCustomSANs = true
	defer srv.Close()

	p2, err := generateAzure()
	assert.FatalError(t, err)
	p2.TenantID = p1.TenantID
	p2.config = p1.config
	p2.oidcConfig = p1.oidcConfig
	p2.keyStore = p1.keyStore
	p2.DisableCustomSANs = false

	p3, err := generateAzure()
	assert.FatalError(t, err)
	// disable sshCA
	disable := false
	p3.Claims = &Claims{EnableSSHCA: &disable}
	p3.claimer, err = NewClaimer(p3.Claims, globalProvisionerClaims)
	assert.FatalError(t, err)

	t1, err := p1.GetIdentityToken("subject", "caURL")
	assert.FatalError(t, err)

	t2, err := p2.GetIdentityToken("subject", "caURL")
	assert.FatalError(t, err)

	key, err := generateJSONWebKey()
	assert.FatalError(t, err)

	signer, err := generateJSONWebKey()
	assert.FatalError(t, err)

	pub := key.Public().Key
	rsa2048, err := rsa.GenerateKey(rand.Reader, 2048)
	assert.FatalError(t, err)
	rsa1024, err := rsa.GenerateKey(rand.Reader, 1024)
	assert.FatalError(t, err)

	hostDuration := p1.claimer.DefaultHostSSHCertDuration()
	expectedHostOptions := &SSHOptions{
		CertType: "host", Principals: []string{"virtualMachine"},
		ValidAfter: NewTimeDuration(tm), ValidBefore: NewTimeDuration(tm.Add(hostDuration)),
	}
	expectedCustomOptions := &SSHOptions{
		CertType: "host", Principals: []string{"foo.bar"},
		ValidAfter: NewTimeDuration(tm), ValidBefore: NewTimeDuration(tm.Add(hostDuration)),
	}

	type args struct {
		token   string
		sshOpts SSHOptions
		key     interface{}
	}
	tests := []struct {
		name        string
		azure       *Azure
		args        args
		expected    *SSHOptions
		code        int
		wantErr     bool
		wantSignErr bool
	}{
		{"ok", p1, args{t1, SSHOptions{}, pub}, expectedHostOptions, http.StatusOK, false, false},
		{"ok-rsa2048", p1, args{t1, SSHOptions{}, rsa2048.Public()}, expectedHostOptions, http.StatusOK, false, false},
		{"ok-type", p1, args{t1, SSHOptions{CertType: "host"}, pub}, expectedHostOptions, http.StatusOK, false, false},
		{"ok-principals", p1, args{t1, SSHOptions{Principals: []string{"virtualMachine"}}, pub}, expectedHostOptions, http.StatusOK, false, false},
		{"ok-options", p1, args{t1, SSHOptions{CertType: "host", Principals: []string{"virtualMachine"}}, pub}, expectedHostOptions, http.StatusOK, false, false},
		{"ok-custom", p2, args{t2, SSHOptions{Principals: []string{"foo.bar"}}, pub}, expectedCustomOptions, http.StatusOK, false, false},
		{"fail-rsa1024", p1, args{t1, SSHOptions{}, rsa1024.Public()}, expectedHostOptions, http.StatusOK, false, true},
		{"fail-type", p1, args{t1, SSHOptions{CertType: "user"}, pub}, nil, http.StatusOK, false, true},
		{"fail-principal", p1, args{t1, SSHOptions{Principals: []string{"smallstep.com"}}, pub}, nil, http.StatusOK, false, true},
		{"fail-extra-principal", p1, args{t1, SSHOptions{Principals: []string{"virtualMachine", "smallstep.com"}}, pub}, nil, http.StatusOK, false, true},
		{"fail-sshCA-disabled", p3, args{"foo", SSHOptions{}, pub}, expectedHostOptions, http.StatusUnauthorized, true, false},
		{"fail-invalid-token", p1, args{"foo", SSHOptions{}, pub}, expectedHostOptions, http.StatusUnauthorized, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.azure.AuthorizeSSHSign(context.Background(), tt.args.token)
			if (err != nil) != tt.wantErr {
				t.Errorf("Azure.AuthorizeSSHSign() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				sc, ok := err.(errs.StatusCoder)
				assert.Fatal(t, ok, "error does not implement StatusCoder interface")
				assert.Equals(t, sc.StatusCode(), tt.code)
				assert.Nil(t, got)
			} else if assert.NotNil(t, got) {
				cert, err := signSSHCertificate(tt.args.key, tt.args.sshOpts, got, signer.Key.(crypto.Signer))
				if (err != nil) != tt.wantSignErr {
					t.Errorf("SignSSH error = %v, wantSignErr %v", err, tt.wantSignErr)
				} else {
					if tt.wantSignErr {
						assert.Nil(t, cert)
					} else {
						assert.NoError(t, validateSSHCertificate(cert, tt.expected))
					}
				}
			}
		})
	}
}

func TestAzure_assertConfig(t *testing.T) {
	p1, err := generateAzure()
	assert.FatalError(t, err)
	p2, err := generateAzure()
	assert.FatalError(t, err)
	p2.config = nil

	tests := []struct {
		name  string
		azure *Azure
	}{
		{"ok with config", p1},
		{"ok no config", p2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.azure.assertConfig()
		})
	}
}
