package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoadValidatesEffectivePort(t *testing.T) {
	for _, test := range []struct {
		name    string
		disk    string
		env     string
		want    int
		wantErr string
	}{
		{name: "default", disk: `{}`, want: 8080},
		{name: "saved zero uses default", disk: `{"port":0}`, want: 8080},
		{name: "saved minimum", disk: `{"port":1}`, want: 1},
		{name: "saved maximum", disk: `{"port":65535}`, want: 65535},
		{name: "saved negative", disk: `{"port":-1}`, wantErr: "port"},
		{name: "saved above maximum", disk: `{"port":65536}`, wantErr: "port"},
		{name: "environment minimum", disk: `{}`, env: "1", want: 1},
		{name: "environment maximum", disk: `{}`, env: "65535", want: 65535},
		{name: "environment overrides saved port", disk: `{"port":9090}`, env: "8081", want: 8081},
		{name: "environment repairs invalid saved port", disk: `{"port":-1}`, env: "8081", want: 8081},
		{name: "empty environment preserves saved port", disk: `{"port":9090}`, want: 9090},
		{name: "environment zero", disk: `{}`, env: "0", wantErr: "PORT"},
		{name: "environment negative", disk: `{}`, env: "-1", wantErr: "PORT"},
		{name: "environment above maximum", disk: `{}`, env: "65536", wantErr: "PORT"},
		{name: "environment noninteger", disk: `{}`, env: "invalid", wantErr: "PORT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := isolatedLoadHome(t)
			writeLoadConfig(t, home, test.disk)
			t.Setenv("PORT", test.env)
			cfg, _, err := Load()
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Load error = %v, want error containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil || cfg.Port != test.want {
				t.Fatalf("Load port = %d, error = %v; want %d", cfg.Port, err, test.want)
			}
		})
	}
}

func TestDuplicateConfigFieldCheckSkipsCompleteValues(t *testing.T) {
	// The preflight checks keys without interpreting their values. Config decoding
	// remains responsible for field types and unknown keys.
	for _, value := range []string{
		`null`, `true`, `123456789012345678901234567890`, `"escaped \\"`,
		`[]`, `{}`, `{"nested":[{"x":"}"},[1,2],true,null]}`,
	} {
		t.Run(value, func(t *testing.T) {
			unique := fmt.Sprintf(`{"one":%s,"two":false}`, value)
			if err := rejectDuplicateConfigFields([]byte(unique)); err != nil {
				t.Fatalf("check unique fields: %v", err)
			}
			duplicate := fmt.Sprintf(`{"one":%s,"ONE":false}`, value)
			if err := rejectDuplicateConfigFields([]byte(duplicate)); err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("check duplicate after nested value = %v", err)
			}
		})
	}
}
