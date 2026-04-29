package app

import (
	"sort"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/sorting"
)

// sorterRulesFromConfig converts config.SorterConfig entries into the
// sorting.SorterRule values that the SortStage expects. Only active sorters
// are included; the result is ordered by SorterConfig.Order ascending.
func sorterRulesFromConfig(cfgs []config.SorterConfig) []sorting.SorterRule {
	// Filter active and sort by Order.
	active := make([]config.SorterConfig, 0, len(cfgs))
	for _, c := range cfgs {
		if c.IsActive {
			active = append(active, c)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Order < active[j].Order })

	rules := make([]sorting.SorterRule, 0, len(active))
	for _, c := range active {
		types := make([]sorting.MediaType, len(c.SortType))
		for j, t := range c.SortType {
			types[j] = sorting.MediaType(t)
		}
		rules = append(rules, sorting.SorterRule{
			Name:       c.Name,
			Enabled:    true,
			Categories: c.SortCats,
			Types:      types,
			SortString: c.SortString,
			Min:        int64(c.MinSize),
		})
	}
	return rules
}
