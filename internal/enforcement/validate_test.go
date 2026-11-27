package enforcement

import (
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
)

// An invalid enforcement rule used to be found only after the Lease was
// taken. ValidateConfig finds everything enforcement would refuse from the
// configuration alone, in the words start-up prints.
func TestValidateConfigFindsWhatStartupWouldRefuse(t *testing.T) {
	good := func() *config.Config {
		return &config.Config{EnforcementMode: "enforce", TenantAPI: true, EnforcementInCluster: true,
			EnforcementRules: "alerts.low_remaining_commit_balance_reached=paused, alert:Wallet below $50=warning,alert:a=b=suspended",
			PauseKeepBilling: "storage,load_balancers", PauseDrainTimeout: 2 * time.Minute,
			EnforcementLiteLLM: true, RecordsPullLiteLLMURL: "http://litellm:4000", RecordsPullLiteLLMKey: "sk-admin"}
	}
	if err := ValidateConfig(good()); err != nil {
		t.Fatalf("a valid configuration: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
		want   string
	}{
		{"a rule with a state that does not exist", func(c *config.Config) { c.EnforcementRules = "alert:vbtest-bad=frozen" },
			`ENFORCEMENT_RULES: enforcement rule "alert:vbtest-bad=frozen" must be event.type=state or alert:<notification ID or name>=state (states: active, warning, delinquent, suspended, paused)`},
		{"a rule without a state", func(c *config.Config) { c.EnforcementRules = "invoice.overdue=suspended,invoice.paid" },
			`ENFORCEMENT_RULES: enforcement rule "invoice.paid" must be event.type=state or alert:<notification ID or name>=state (states: active, warning, delinquent, suspended, paused)`},
		{"a rule that names no notification", func(c *config.Config) { c.EnforcementRules = "alert:=paused" },
			`ENFORCEMENT_RULES: enforcement rule "alert:=paused" must be event.type=state or alert:<notification ID or name>=state (states: active, warning, delinquent, suspended, paused)`},
		{"a mode that does not exist", func(c *config.Config) { c.EnforcementMode = "block" },
			`ENFORCEMENT_MODE "block" must be observe, annotate or enforce`},
		{"in-cluster enforcement without cluster APIs", func(c *config.Config) { c.TenantAPI = false },
			`ENFORCEMENT_IN_CLUSTER needs TENANT_API=true`},
		{"the LiteLLM gateway without its key", func(c *config.Config) { c.RecordsPullLiteLLMKey = "" },
			`ENFORCEMENT_LITELLM needs LITELLM_URL and LITELLM_API_KEY (a LiteLLM admin key)`},
		{"an unknown kind of capacity to keep billing", func(c *config.Config) { c.PauseKeepBilling = "storage,gpus" },
			`PAUSE_KEEP_BILLING: unknown "gpus" (use storage, load_balancers, nodes, control_plane, workloads, or none)`},
		{"no drain timeout", func(c *config.Config) { c.PauseDrainTimeout = 0 },
			`PAUSE_DRAIN_TIMEOUT 0s must be positive`},
	} {
		cfg := good()
		tc.change(cfg)
		err := ValidateConfig(cfg)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v\nwant: %s", tc.name, err, tc.want)
		}
		if again := ValidateConfig(cfg); again == nil || again.Error() != tc.want {
			t.Errorf("%s: a second call answered %v", tc.name, again)
		}
	}
	// What vBilling starts with when nothing is set is valid.
	if err := ValidateConfig(config.Load()); err != nil {
		t.Fatalf("the default configuration: %v", err)
	}
}
