package ignore

import (
	"context"
	"net/url"
	"time"

	"go.skia.org/infra/go/skerr"
)

// Store is an interface for a database that saves ignore rules.
type Store interface {
	// Create adds a new rule to the ignore store. The ID will be set if this call is successful.
	Create(context.Context, *Rule) error

	// List returns all ignore rules in the ignore store.
	List(context.Context) ([]*Rule, error)

	// Update sets a Rule.
	Update(ctx context.Context, rule *Rule) error

	// Delete removes a Rule from the store. The return value is the number of
	// records that were deleted (either 0 or 1).
	Delete(ctx context.Context, id string) (int, error)
}

// Rule defines a single ignore rule, matching zero or more traces based on
// Query.
type Rule struct {
	ID        string
	Name      string
	UpdatedBy string
	Expires   time.Time
	Query     string
	Note      string
}

// toQuery makes a slice of url.Values from the given slice of Rules.
func toQuery(ignores []*Rule) ([]url.Values, error) {
	var ret []url.Values
	for _, ignore := range ignores {
		v, err := url.ParseQuery(ignore.Query)
		if err != nil {
			return nil, skerr.Wrapf(err, "invalid ignore rule id %q; query %q", ignore.ID, ignore.Query)
		}
		ret = append(ret, v)
	}
	return ret, nil
}

// NewRule creates a new ignore rule with the given data.
func NewRule(createdByUser string, expires time.Time, queryStr string, note string) *Rule {
	return &Rule{
		Name:      createdByUser,
		UpdatedBy: createdByUser,
		Expires:   expires,
		Query:     queryStr,
		Note:      note,
	}
}
