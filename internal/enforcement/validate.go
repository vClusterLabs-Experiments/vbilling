package enforcement

import (
	"errors"
	"fmt"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// ValidateConfig checks everything enforcement reads from the configuration:
// ENFORCEMENT_MODE, ENFORCEMENT_RULES, ENFORCEMENT_ESCALATIONS,
// ENFORCEMENT_IN_CLUSTER, ENFORCEMENT_LITELLM (with LITELLM_URL and
// LITELLM_API_KEY), PAUSE_KEEP_BILLING and the pause drain timeout. It
// reads nothing but cfg: no cluster, no file, no network, and it changes
// nothing. It returns the first problem, worded as start-up reports it, or
// nil.
//
// vBilling calls it before it takes its Lease, so a setting enforcement
// cannot use stops a new instance at once instead of after the wait for
// the Lease, with the Lease held by an instance that exits.
func ValidateConfig(cfg *config.Config) error {
	switch cfg.EnforcementMode {
	case "observe", "annotate", "enforce":
	default:
		return fmt.Errorf("ENFORCEMENT_MODE %q must be observe, annotate or enforce", cfg.EnforcementMode)
	}
	if _, err := ParseRules(cfg.EnforcementRules); err != nil {
		return fmt.Errorf("ENFORCEMENT_RULES: %w", err)
	}
	if _, err := ParseEscalations(cfg.EnforcementEscalations); err != nil {
		return fmt.Errorf("ENFORCEMENT_ESCALATIONS: %w", err)
	}
	if cfg.EnforcementInCluster && !cfg.TenantAPI {
		return errors.New("ENFORCEMENT_IN_CLUSTER needs TENANT_API=true")
	}
	if cfg.EnforcementLiteLLM && (cfg.RecordsPullLiteLLMURL == "" || cfg.RecordsPullLiteLLMKey == "") {
		return errors.New("ENFORCEMENT_LITELLM needs LITELLM_URL and LITELLM_API_KEY (a LiteLLM admin key)")
	}
	if _, err := pause.ParseKeep(cfg.PauseKeepBilling); err != nil {
		return err
	}
	if cfg.PauseDrainTimeout <= 0 {
		return fmt.Errorf("PAUSE_DRAIN_TIMEOUT %s must be positive", cfg.PauseDrainTimeout)
	}
	return nil
}
