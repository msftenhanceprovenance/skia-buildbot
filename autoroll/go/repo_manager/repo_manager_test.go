package repo_manager

import (
	"testing"

	assert "github.com/stretchr/testify/require"
	"go.skia.org/infra/autoroll/go/strategy"
	"go.skia.org/infra/go/testutils"
)

func validCommonBaseConfig() *CommonRepoManagerConfig {
	return &CommonRepoManagerConfig{
		ChildBranch:  "childBranch",
		ChildPath:    "childPath",
		ParentBranch: "parentBranch",
		Strategy:     strategy.ROLL_STRATEGY_BATCH,
	}
}

func TestCommonConfigValidation(t *testing.T) {
	testutils.SmallTest(t)

	assert.NoError(t, validCommonBaseConfig().Validate())
	cfg := validCommonBaseConfig()
	cfg.PreUploadSteps = []string{"TrainInfra"}
	assert.NoError(t, cfg.Validate())

	// Helper function: create a valid base config, allow the caller to
	// mutate it, then assert that validation fails with the given message.
	testErr := func(fn func(c *CommonRepoManagerConfig), err string) {
		c := validCommonBaseConfig()
		fn(c)
		assert.EqualError(t, c.Validate(), err)
	}

	// Test cases.

	testErr(func(c *CommonRepoManagerConfig) {
		c.ChildBranch = ""
	}, "ChildBranch is required.")

	testErr(func(c *CommonRepoManagerConfig) {
		c.ChildPath = ""
	}, "ChildPath is required.")

	testErr(func(c *CommonRepoManagerConfig) {
		c.ParentBranch = ""
	}, "ParentBranch is required.")

	testErr(func(c *CommonRepoManagerConfig) {
		c.Strategy = ""
	}, "Strategy is required.")

	testErr(func(c *CommonRepoManagerConfig) {
		c.Strategy = "bogus"
	}, "Unknown roll strategy \"bogus\"")

	testErr(func(c *CommonRepoManagerConfig) {
		c.PreUploadSteps = []string{
			"bogus",
		}
	}, "No such pre-upload step: bogus")
}

func TestDepotToolsConfigValidation(t *testing.T) {
	testutils.SmallTest(t)

	validBaseConfig := func() *DepotToolsRepoManagerConfig {
		return &DepotToolsRepoManagerConfig{
			CommonRepoManagerConfig: *validCommonBaseConfig(),
			ParentRepo:              "parentRepo",
		}
	}

	assert.NoError(t, validBaseConfig().Validate())
	cfg := validBaseConfig()
	cfg.GClientSpec = "dummy"
	assert.NoError(t, cfg.Validate())

	cfg.ParentRepo = ""
	assert.EqualError(t, cfg.Validate(), "ParentRepo is required.")

	// Verify that the CommonRepoManagerConfig gets validated.
	cfg = &DepotToolsRepoManagerConfig{
		ParentRepo: "parentRepo",
	}
	assert.Error(t, cfg.Validate())
}
