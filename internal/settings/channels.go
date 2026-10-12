package settings

import (
	"maps"
	"slices"
	"strings"
)

// WarningChannelUnreadable identifies the warning that notification channels with
// queued deliveries cannot be loaded (their secrets cannot be decrypted, or their
// stored row is damaged): their deliveries fail and are given up after the usual
// rounds. It names the channels; re-saving (or deleting) a channel resolves it.
const WarningChannelUnreadable = "notification_channel_unreadable"

// SetChannelUnreadable shows the WarningChannelUnreadable warning for channel id
// with problem (redacted), or removes the channel from it with an empty problem.
// The state is not stored: the notification service reports it while it runs.
func (s *Service) SetChannelUnreadable(id, problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if problem == "" {
		delete(s.unreadableChannels, id)
		return
	}
	if s.unreadableChannels == nil {
		s.unreadableChannels = map[string]string{}
	}
	s.unreadableChannels[id] = problem
}

// channelUnreadableWarning returns the WarningChannelUnreadable warning, if any
// channel is unreadable. Caller holds s.mu (read).
func (s *Service) channelUnreadableWarning() (Warning, bool) {
	if len(s.unreadableChannels) == 0 {
		return Warning{}, false
	}
	ids := slices.Sorted(maps.Keys(s.unreadableChannels))
	return Warning{ID: WarningChannelUnreadable, Setting: "notifications", Channels: ids,
		Message: "These notification channels cannot be loaded, so their notifications fail: " + strings.Join(ids, ", ") +
			" (" + s.unreadableChannels[ids[0]] + "). Re-enter their secrets (edit and save the channel) or delete them."}, true
}
