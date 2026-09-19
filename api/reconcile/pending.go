package reconcile

import (
	"context"

	"github.com/datapointchris/ypl/api/store"
	"github.com/datapointchris/ypl/api/store/generated"
)

// readPending is store.PendingPushes as of now, read in one read transaction.
func (r *Runner) readPending(ctx context.Context) ([]store.PendingPush, error) {
	var pending []store.PendingPush
	err := r.store.InReadTx(ctx, func(q *generated.Queries) error {
		var err error
		pending, err = store.PendingPushes(ctx, q, r.now())
		return err
	})
	return pending, err
}
