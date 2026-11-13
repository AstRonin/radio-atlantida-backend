package playrules

import (
	"context"
	"fmt"

	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
)

// GuardRetention registers the check that refuses a play log retention
// shorter than the longest play rule interval (design §4.1).
func (s *Service) GuardRetention(set *settings.Service) {
	settings.Guard(set, settings.RetentionPlayLogDays, func(ctx context.Context, days int) error {
		longest, err := s.LongestIntervalMinutes(ctx)
		if err != nil {
			return err
		}
		if longest > days*24*60 {
			return settings.Refuse(settings.RetentionPlayLogDays.Name(), settings.RuleIntervalBeyondRetention,
				fmt.Sprintf("a play rule has an interval of %d minutes; the play log must keep at least %d days",
					longest, (longest+24*60-1)/(24*60)))
		}
		return nil
	})
}
