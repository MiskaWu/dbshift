package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestPasswordsFromEnvAreUsedButNotShown(t *testing.T) {
	t.Setenv("MYSQL_PASS", "mysql-secret-value")
	t.Setenv("PG_PASS", "pg-secret-value")

	saved := *cfg
	t.Cleanup(func() { *cfg = saved })

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	addFlags(fs)

	if cfg.MySQL.Pass != "mysql-secret-value" || cfg.PG.Pass != "pg-secret-value" {
		t.Errorf("passwords from the environment were not applied: %q, %q", cfg.MySQL.Pass, cfg.PG.Pass)
	}
	usage := fs.FlagUsages()
	for _, secret := range []string{"mysql-secret-value", "pg-secret-value"} {
		if strings.Contains(usage, secret) {
			t.Errorf("usage text contains %q:\n%s", secret, usage)
		}
	}
}
