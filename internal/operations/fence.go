package operations

import "fmt"

// checkFence returns ErrNotActive once the HA fence (Config.Fence) fails: this
// instance no longer holds the lease, and must change nothing. It is called right
// before a synchronous change is committed; nil without a fence.
func (s *Service) checkFence() error {
	if s.cfg.Fence == nil {
		return nil
	}
	if err := s.cfg.Fence.Check(); err != nil {
		return fmt.Errorf("%w: %w", ErrNotActive, err)
	}
	return nil
}
