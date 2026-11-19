package paygateway

import (
	"fmt"
	"log/slog"
	"time"

	"rightproperty365.com/backend/internal/platform/config"
)

// Open returns the configured gateway: nil for none (online payment is not
// offered). The fake driver is refused in production (config.FromLookup
// refuses it too).
func Open(cfg config.Config, logger *slog.Logger, now func() time.Time) (Gateway, error) {
	switch cfg.PaymentGateway {
	case "", config.PaymentGatewayNone:
		return nil, nil
	case config.PaymentGatewayFake:
		secret := []byte(cfg.PaymentFakeWebhookSecret.Reveal())
		f, err := NewFake(cfg.Env, secret, now)
		if err != nil {
			return nil, err
		}
		if logger != nil {
			logger.Warn("online payment uses the FAKE gateway: nothing is charged; development and staging only",
				slog.Bool("webhook_secret_configured", len(secret) > 0))
		}
		return f, nil
	}
	return nil, fmt.Errorf("paygateway: unknown PAYMENT_GATEWAY %q", cfg.PaymentGateway)
}
