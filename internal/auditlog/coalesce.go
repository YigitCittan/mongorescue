package auditlog

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// Targets of a summary entry (see window.summary).
const (
	// TargetCoalescedFrom and TargetCoalescedUntil bound the refusals a summary
	// entry stands for.
	TargetCoalescedFrom  = "coalesced_from"
	TargetCoalescedUntil = "coalesced_until"
	// TargetNamePrefix numbers the distinct attempted names ("name_1" ...).
	TargetNamePrefix = "name_"
	// TargetNamesMore counts the refusals whose attempted name was not kept.
	TargetNamesMore = "names_more"
)

// window coalesces the identical refusals of one caller: the first is stored at
// once, the following ones within CoalesceWindow are counted, and a summary entry
// with the count is written when the window closes.
type window struct {
	// first is the stored refusal that opened the window.
	first Event
	// since is its time, last the time of the latest counted refusal.
	since, last time.Time
	// suppressed counts the refusals not stored one by one.
	suppressed int
	// names are the distinct attempted names of the counted refusals (at most
	// MaxSummaryNames); moreNames counts the refusals whose name was not kept.
	names     []string
	moreNames int
}

// isRefusal reports whether e is coalesced.
func isRefusal(e *Event) bool {
	return e.Outcome == OutcomeDenied || e.Outcome == OutcomeRateLimited
}

// refusalKey identifies the caller and refusal e belongs to. Anonymous refusals
// (failed sign-ins, missing credentials) are keyed by client address, action and
// outcome only: the attempted name is attacker-chosen and must not open new windows.
func refusalKey(e *Event) string {
	name := e.ActorName
	if e.ActorKind == ActorAnonymous {
		name = ""
	}
	return strings.Join([]string{e.ActorKind, e.ActorUserID, name, e.ActorKeyID, e.ClientIP, e.Action, e.Outcome, strconv.Itoa(e.Status)}, "\x00")
}

// add counts e into the window.
func (w *window) add(e *Event) {
	w.suppressed += e.Count
	if e.Time.After(w.last) {
		w.last = e.Time
	}
	if e.ActorKind != ActorAnonymous || e.ActorName == "" {
		return
	}
	name := clean(e.ActorName, maxSummaryNameLength)
	switch {
	case slices.Contains(w.names, name):
	case len(w.names) < MaxSummaryNames:
		w.names = append(w.names, name)
	default:
		w.moreNames++
	}
}

// summary returns the entry recording the counted refusals, or false when there
// were none. It carries the first refusal's actor, action, status and client, the
// count, the time range and, for anonymous refusals, the attempted names.
func (w *window) summary() (Event, bool) {
	if w.suppressed == 0 {
		return Event{}, false
	}
	e := w.first
	e.Time, e.Count = w.last, w.suppressed
	e.Targets = map[string]string{
		TargetCoalescedFrom:  FormatTime(w.since),
		TargetCoalescedUntil: FormatTime(w.last),
	}
	if e.ActorKind == ActorAnonymous {
		e.ActorName = ""
		for i, n := range w.names {
			e.Targets[TargetNamePrefix+strconv.Itoa(i+1)] = n
		}
		if w.moreNames > 0 {
			e.Targets[TargetNamesMore] = strconv.Itoa(w.moreNames)
		}
	}
	normalize(&e)
	return e, true
}

// coalesce reports whether the refusal e is counted into an open window instead of
// stored. A refusal opening a window is stored; the summary of the window it
// replaces, and of every window when the index is full, is written first.
func (s *Service) coalesce(e *Event) bool {
	if !isRefusal(e) {
		return false
	}
	key := refusalKey(e)
	var flush []Event
	s.mu.Lock()
	w, ok := s.windows[key]
	if ok && !e.Time.Before(w.since) && e.Time.Sub(w.since) < CoalesceWindow {
		w.add(e)
		s.mu.Unlock()
		return true
	}
	if ok {
		if sum, has := w.summary(); has {
			flush = append(flush, sum)
		}
		delete(s.windows, key)
	}
	if len(s.windows) >= maxCoalesceKeys {
		flush = append(flush, s.takeWindows(func(*window) bool { return true })...)
	}
	s.windows[key] = &window{first: *e, since: e.Time, last: e.Time}
	s.mu.Unlock()
	for _, sum := range flush {
		s.submit(sum)
	}
	return false
}

// takeWindows removes the windows matching closed and returns their summaries,
// oldest first. The caller holds s.mu.
func (s *Service) takeWindows(closed func(*window) bool) []Event {
	var out []Event
	for k, w := range s.windows {
		if !closed(w) {
			continue
		}
		if sum, ok := w.summary(); ok {
			out = append(out, sum)
		}
		delete(s.windows, k)
	}
	slices.SortFunc(out, func(a, b Event) int { return a.Time.Compare(b.Time) })
	return out
}

// flushRefusals writes the summaries of the windows closed at now (all of them with
// all) and forgets those windows.
func (s *Service) flushRefusals(now time.Time, all bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	out := s.takeWindows(func(w *window) bool {
		return all || now.Sub(w.since) >= CoalesceWindow || now.Before(w.since)
	})
	s.mu.Unlock()
	for _, e := range out {
		s.submit(e)
	}
}

// Flush writes the summaries of every open coalescing window. Run does this when it
// stops; call it directly when the service is used without Run.
func (s *Service) Flush() {
	if s == nil || s.cfg.Repo == nil {
		return
	}
	s.flushRefusals(time.Time{}, true)
}
