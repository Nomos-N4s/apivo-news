package account

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestAccountFrom(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	wantAccount := Account{ID: id}

	t.Run("valid account in context", func(t *testing.T) {
		t.Parallel()
		ctx := context.WithValue(context.Background(), ctxKey{}, wantAccount)
		got := accountFrom(ctx)
		if got != wantAccount {
			t.Errorf("accountFrom() = %+v, want %+v", got, wantAccount)
		}
	})

	t.Run("empty context", func(t *testing.T) {
		t.Parallel()
		got := accountFrom(context.Background())
		if got != (Account{}) {
			t.Errorf("accountFrom() = %+v, want zero value Account{}", got)
		}
	})

	t.Run("wrong value type under ctxKey", func(t *testing.T) {
		t.Parallel()
		ctx := context.WithValue(context.Background(), ctxKey{}, "invalid-type")
		got := accountFrom(ctx)
		if got != (Account{}) {
			t.Errorf("accountFrom() = %+v, want zero value Account{}", got)
		}
	})
}
