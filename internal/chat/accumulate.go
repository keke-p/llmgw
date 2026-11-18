package chat

import (
	"sort"
	"strings"
	"time"
)

// Accumulator assembles streamed events into a complete response. It is used to
// build non-streaming responses and request logs.
type Accumulator struct {
	ID      string
	Created int64
	Usage   *Usage
	// FirstTokenAt is set by the engine when the first token arrives (used for
	// timing statistics in non-streaming responses).
	FirstTokenAt time.Time
	choices      map[int]*Choice
}

// Choice is an assembled choice.
type Choice struct {
	Index        int
	Role         string
	content      strings.Builder
	reasoning    strings.Builder
	toolCalls    map[int]*ToolCall
	FinishReason string
	StopSequence string
}

func (c *Choice) Content() string   { return c.content.String() }
func (c *Choice) Reasoning() string { return c.reasoning.String() }

// ToolCalls returns the assembled tool calls ordered by index.
func (c *Choice) ToolCalls() []ToolCall {
	if len(c.toolCalls) == 0 {
		return nil
	}
	idx := make([]int, 0, len(c.toolCalls))
	for i := range c.toolCalls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]ToolCall, 0, len(idx))
	for _, i := range idx {
		out = append(out, *c.toolCalls[i])
	}
	return out
}

// NewAccumulator returns an empty accumulator.
func NewAccumulator() *Accumulator {
	return &Accumulator{choices: map[int]*Choice{}}
}

// Reset discards everything (used before a fallback attempt).
func (a *Accumulator) Reset() {
	*a = Accumulator{choices: map[int]*Choice{}}
}

// Add folds an event into the accumulator.
func (a *Accumulator) Add(ev *Event) {
	if ev.ID != "" && a.ID == "" {
		a.ID = ev.ID
	}
	if ev.Created != 0 && a.Created == 0 {
		a.Created = ev.Created
	}
	if ev.Usage != nil {
		u := *ev.Usage
		a.Usage = &u
	}
	if !ev.IsToken() && ev.FinishReason == "" && ev.Role == "" {
		return
	}
	c := a.choice(ev.Choice)
	if ev.Role != "" {
		c.Role = ev.Role
	}
	c.content.WriteString(ev.Content)
	c.reasoning.WriteString(ev.Reasoning)
	for _, d := range ev.ToolCalls {
		if c.toolCalls == nil {
			c.toolCalls = map[int]*ToolCall{}
		}
		tc := c.toolCalls[d.Index]
		if tc == nil {
			tc = &ToolCall{}
			c.toolCalls[d.Index] = tc
		}
		if d.ID != "" {
			tc.ID = d.ID
		}
		if d.Name != "" {
			tc.Name += d.Name
		}
		tc.Arguments += d.Arguments
	}
	if ev.FinishReason != "" {
		c.FinishReason = ev.FinishReason
	}
	if ev.StopSequence != "" {
		c.StopSequence = ev.StopSequence
	}
}

func (a *Accumulator) choice(i int) *Choice {
	c := a.choices[i]
	if c == nil {
		c = &Choice{Index: i, Role: "assistant"}
		a.choices[i] = c
	}
	return c
}

// Choices returns assembled choices ordered by index. There is always at least
// one choice.
func (a *Accumulator) Choices() []*Choice {
	if len(a.choices) == 0 {
		a.choice(0)
	}
	out := make([]*Choice, 0, len(a.choices))
	for _, c := range a.choices {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// HasOutput reports whether anything was generated.
func (a *Accumulator) HasOutput() bool {
	for _, c := range a.choices {
		if c.content.Len() > 0 || c.reasoning.Len() > 0 || len(c.toolCalls) > 0 {
			return true
		}
	}
	return false
}
