package config

import "testing"

func TestValidateCloudEndpointRequiresTLSExceptLoopback(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		wantErr  bool
	}{
		{name: "production TLS", endpoint: "https://cloudcode-pa.googleapis.com"},
		{name: "local IPv4 test server", endpoint: "http://127.0.0.1:19090"},
		{name: "local hostname test server", endpoint: "http://localhost:19090"},
		{name: "remote plaintext endpoint", endpoint: "http://example.com", wantErr: true},
		{name: "unsupported scheme", endpoint: "ftp://example.com", wantErr: true},
		{name: "embedded credentials", endpoint: "https://user:pass@example.com", wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := validateCloudEndpoint("ENDPOINT", test.endpoint)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateCloudEndpoint(%q) error = %v, wantErr %t", test.endpoint, err, test.wantErr)
			}
		})
	}
}

func TestValidateListenAddressRequiresKeyOffLoopback(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		apiKey  string
		wantErr bool
	}{
		{name: "default IPv4", host: "127.0.0.1"},
		{name: "localhost", host: "localhost"},
		{name: "IPv6 loopback", host: "::1"},
		{name: "public listener without key", host: "0.0.0.0", wantErr: true},
		{name: "public listener with key", host: "0.0.0.0", apiKey: "secret"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := validateListenAddress(test.host, test.apiKey)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateListenAddress(%q, API key present %t) error = %v, wantErr %t", test.host, test.apiKey != "", err, test.wantErr)
			}
		})
	}
}
