//go:build dev

package guest

// MaxHarvestAnswer caps harvest's answer, which carries the agent's seeded and created skill
// files as base64.
const MaxHarvestAnswer = 16 << 20

func init() { fileJobs["harvest"] = MaxHarvestAnswer }
