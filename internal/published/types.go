package published

// Type is an agent type troupe creates, as its published kit declares it today: its default
// sizes (defaults.json), and what create asks for, in order (settings.json, then
// services.json, then the Slack manager when the kit has Slack). Help is rendered from it, so
// create notices when a pulled kit no longer matches (Differs).
type Type struct {
	Name, Summary string
	Defaults      Defaults
	Soul          string // the default SOUL.md, in words
	Asks          []Ask
}

// Ask is one value create asks for: Name is a setting's variable, a service's name, or
// "manager"; Label is how help names it, when not Name.
type Ask struct {
	Name, Label, Desc string
	Optional          bool
}

// Types are the agent types, in the order help lists them.
var Types = []Type{
	{
		Name:     "coder",
		Summary:  "A coding agent that works through Slack and GitHub.",
		Defaults: Defaults{CPUs: 8, MemoryGiB: 12, RootGiB: 100, DockerGiB: 20},
		Soul:     "coder's built-in SOUL",
		Asks: []Ask{
			{Name: "GITHUB_ORGANIZATIONS", Label: "organizations", Desc: "GitHub organizations it may work in, as logins.", Optional: true},
			{Name: "venice", Desc: "Venice API key."},
			{Name: "github", Desc: "GitHub token of the agent's own account."},
			{Name: "starkscan", Desc: "Starkscan API key."},
			{Name: "slack-app", Desc: "Slack app-level token (xapp-…), after the Slack app's setup link."},
			{Name: "slack-bot", Desc: "Slack bot token (xoxb-…)."},
			{Name: "manager", Desc: "The manager's Slack member ID (U…), usually yours."},
		},
	},
	{
		Name:     "hermes",
		Summary:  "Plain Hermes, with no Slack or GitHub.",
		Defaults: Defaults{CPUs: 4, MemoryGiB: 6, RootGiB: 50, DockerGiB: 10},
		Soul:     "Hermes's own",
		Asks: []Ask{
			{Name: "venice", Desc: "Venice API key."},
		},
	},
}

// TypeNamed is the type with that name, if troupe creates it.
func TypeNamed(name string) (Type, bool) {
	for _, t := range Types {
		if t.Name == name {
			return t, true
		}
	}
	return Type{}, false
}

// Differs reports whether a pulled kit's sizes, or what it asks for, are not what the type
// says: each value's name and whether it is optional, in order.
func (t Type) Differs(sizes Defaults, asks []Ask) bool {
	if sizes != t.Defaults || len(asks) != len(t.Asks) {
		return true
	}
	for i, a := range t.Asks {
		if a.Name != asks[i].Name || a.Optional != asks[i].Optional {
			return true
		}
	}
	return false
}

// Coder is the coder type, whose size doctor checks a machine can hold.
func Coder() Type {
	t, _ := TypeNamed("coder")
	return t
}
