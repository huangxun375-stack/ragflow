package config

import (
	"testing"

	"github.com/spf13/viper"
)

func TestCodeArchiveBackendScope(t *testing.T) {
	for _, tc := range []struct {
		database, engine string
		want             bool
	}{
		{"mysql", "elasticsearch", true},
		{"mysql", "infinity", false},
		{"mysql", "oceanbase", false},
		{"mysql", "seekdb", false},
		{"mysql", "serenedb", false},
		{"mysql", "vastbase", false},
		{"oceanbase", "elasticsearch", false},
		{"vastbase", "elasticsearch", false},
	} {
		t.Run(tc.database+"/"+tc.engine, func(t *testing.T) {
			v := viper.New()
			v.Set("general.database", tc.database)
			v.Set("general.doc_engine", tc.engine)
			c := &Config{}
			if err := c.ParseGeneralConfig(v); err != nil {
				t.Fatal(err)
			}
			if got := c.SupportsCodeArchive(); got != tc.want {
				t.Fatalf("supported = %t", got)
			}
		})
	}
}

func TestCodeArchiveBackendScopeUsesEffectiveEnvironmentOverrides(t *testing.T) {
	v := viper.New()
	c := &Config{}
	if err := c.ParseGeneralConfig(v); err != nil {
		t.Fatal(err)
	}
	c.environments.DocumentEngineType = "infinity"
	if c.SupportsCodeArchive() {
		t.Fatal("environment engine override ignored")
	}
	c.environments.DocumentEngineType = "elasticsearch"
	c.environments.DatabaseType = "oceanbase"
	if c.SupportsCodeArchive() {
		t.Fatal("environment database override ignored")
	}
}
