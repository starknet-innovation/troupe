// Package progress prints operator lines, one readable sentence per event:
//
//	[HH:MM:SS] <Agent> <step>: <status>[ in <duration>]. <Message>.
//
// Status is started, ok, skipped, waiting, ACTION or FAILED, or empty for a plain note.
// ok and FAILED after a started line add the step's duration. FAILED goes to stderr.
// Terminal output is never JSON or native tool chatter; that goes to a private log.
package progress

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"time"
)

// Statuses a line may carry.
const (
	Started = "started"
	OK      = "ok"
	Skipped = "skipped"
	Waiting = "waiting"
	Action  = "ACTION"
	Failed  = "FAILED"
	// Info is a step's note without a status of its own.
	Info = ""
)

// Printer writes progress lines for one agent.
type Printer struct {
	Agent    string
	Location *time.Location
	Out, Err io.Writer
	// Now is the clock; tests replace it.
	Now     func() time.Time
	started map[string]time.Time
}

// New prints for agent in loc to stdout and stderr.
func New(agent string, loc *time.Location) *Printer {
	return &Printer{Agent: agent, Location: loc, Out: os.Stdout, Err: os.Stderr, Now: time.Now}
}

var endsSentence = regexp.MustCompile(`[.!?:]$`)

// Line formats one progress line and records step timers.
func (p *Printer) Line(step, status, message string) string {
	now := p.Now()
	text := fmt.Sprintf("[%s] %s %s: ", now.In(p.Location).Format("15:04:05"), p.Agent, step)
	if status != "" {
		text += status
		if status == Started {
			if p.started == nil {
				p.started = map[string]time.Time{}
			}
			p.started[step] = now
		} else if begin, ok := p.started[step]; ok && (status == OK || status == Failed) {
			text += " in " + Duration(now.Sub(begin))
			delete(p.started, step)
		}
		text += "."
		if message != "" {
			text += " "
		}
	}
	if message != "" {
		text += message
		// ACTION lines quote exact input ("send exactly: !status"): never append to it.
		if status != Action && !endsSentence.MatchString(message) {
			text += "."
		}
	}
	return text
}

// Print writes one line, FAILED to stderr.
func (p *Printer) Print(step, status, message string) {
	stream := p.Out
	if status == Failed {
		stream = p.Err
	}
	fmt.Fprintln(stream, p.Line(step, status, message))
}

// Duration renders whole seconds as 42s, 3m 5s or 2h 7m.
func Duration(d time.Duration) string {
	s := int(d / time.Second)
	switch {
	case s >= 3600:
		return fmt.Sprintf("%dh %dm", s/3600, s%3600/60)
	case s >= 60:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
