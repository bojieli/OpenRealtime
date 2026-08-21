package cascade_test

import (
	"github.com/bojieli/OpenRealtime/interaction"
)

func defaultPolicies() interaction.Policies { return interaction.Defaults() }

func parseRollout(level string) (interaction.Rollout, error) {
	return interaction.ParseRollout(level, interaction.RolloutOptions{})
}
