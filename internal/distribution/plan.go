package distribution

import (
	"cmp"
	"slices"

	"github.com/webitel/webitel-emails/internal/model"
)

// plan lists the ownership changes needed for an even distribution.
type plan struct {
	assign   []assignment
	unassign []int64
}

type assignment struct {
	profileID  int64
	instanceID string
}

// planDistribution gives every instance ⌊N/K⌋ or ⌈N/K⌉ enabled profiles, moving as few as possible.
func planDistribution(instances []string, profiles []*model.EmailProfileOwnership) plan {
	var result plan

	instances = slices.Compact(slices.Sorted(slices.Values(instances)))
	if len(instances) == 0 {
		return result
	}

	load := make(map[string]int, len(instances))
	for _, id := range instances {
		load[id] = 0
	}

	enabled := make([]*model.EmailProfileOwnership, 0, len(profiles))
	for _, profile := range profiles {
		if !profile.Enabled {
			if profile.OwnerInstanceID != "" {
				result.unassign = append(result.unassign, profile.ProfileID)
			}

			continue
		}

		enabled = append(enabled, profile)
		if _, live := load[profile.OwnerInstanceID]; live {
			load[profile.OwnerInstanceID]++
		}
	}

	slices.SortFunc(enabled, func(a, b *model.EmailProfileOwnership) int {
		return cmp.Compare(a.ProfileID, b.ProfileID)
	})

	// The most loaded instances get the extra slot, so fewer profiles move.
	byLoad := slices.Clone(instances)
	slices.SortStableFunc(byLoad, func(a, b string) int {
		return cmp.Compare(load[b], load[a])
	})

	base, extra := len(enabled)/len(instances), len(enabled)%len(instances)
	quota := make(map[string]int, len(instances))
	for i, id := range byLoad {
		quota[id] = base
		if i < extra {
			quota[id]++
		}
	}

	kept := make(map[string]int, len(instances))
	pending := make([]int64, 0)
	for _, profile := range enabled {
		owner := profile.OwnerInstanceID
		if q, live := quota[owner]; live && kept[owner] < q {
			kept[owner]++

			continue
		}

		pending = append(pending, profile.ProfileID)
	}

	for _, profileID := range pending {
		target := instances[0]
		for _, id := range instances[1:] {
			if quota[id]-kept[id] > quota[target]-kept[target] {
				target = id
			}
		}

		kept[target]++
		result.assign = append(result.assign, assignment{profileID: profileID, instanceID: target})
	}

	return result
}
