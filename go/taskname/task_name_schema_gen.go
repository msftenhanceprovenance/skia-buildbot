// Code generated by "go run gen_schema.go"; DO NOT EDIT

package taskname

var SCHEMA_FROM_GIT = map[string]*Schema{
	"Build":       {Keys: []string{"os", "compiler", "target_arch", "configuration"}, OptionalKeys: []string{"extra_config"}, RecurseRoles: []string(nil)},
	"BuildStats":  {Keys: []string{"os", "compiler", "target_arch", "configuration"}, OptionalKeys: []string{"extra_config"}, RecurseRoles: []string(nil)},
	"Canary":      {Keys: []string{"project"}, OptionalKeys: []string(nil), RecurseRoles: []string(nil)},
	"FM":          {Keys: []string{"os", "compiler", "model", "cpu_or_gpu", "cpu_or_gpu_value", "arch", "configuration", "test_filter"}, OptionalKeys: []string{"extra_config"}, RecurseRoles: []string(nil)},
	"Housekeeper": {Keys: []string{"frequency"}, OptionalKeys: []string{"extra_config"}, RecurseRoles: []string(nil)},
	"Infra":       {Keys: []string{"frequency"}, OptionalKeys: []string{"extra_config"}, RecurseRoles: []string(nil)},
	"Perf":        {Keys: []string{"os", "compiler", "model", "cpu_or_gpu", "cpu_or_gpu_value", "arch", "configuration", "test_filter"}, OptionalKeys: []string{"extra_config"}, RecurseRoles: []string(nil)},
	"Test":        {Keys: []string{"os", "compiler", "model", "cpu_or_gpu", "cpu_or_gpu_value", "arch", "configuration", "test_filter"}, OptionalKeys: []string{"extra_config"}, RecurseRoles: []string(nil)},
	"Upload":      {Keys: []string(nil), OptionalKeys: []string(nil), RecurseRoles: []string{"Build", "BuildStats", "Perf", "Test"}},
}

var SEPARATOR_FROM_GIT = "-"
