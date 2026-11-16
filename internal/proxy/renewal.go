// renewalCycle remembers a published certificate until its activation succeeds. While activation is pending it never initiates another issuance.
package proxy

import (
	"context"
	"fmt"
)

type renewalCycle struct{ activationPending bool }

func (c *renewalCycle) step(ctx context.Context, renew func(context.Context) (bool, error), activate func(context.Context) error) error {
	if !c.activationPending {
		renewed, err := renew(ctx)
		if err != nil {
			return fmt.Errorf("certificate renewal: %w", err)
		}
		if !renewed {
			return nil
		}
		c.activationPending = true
	}
	if err := activate(ctx); err != nil {
		return fmt.Errorf("certificate activation: %w", err)
	}
	c.activationPending = false
	return nil
}
