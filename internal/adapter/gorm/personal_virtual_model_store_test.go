package gorm_test

import (
	"context"
	"testing"

	xologorm "github.com/xolo-gateway/xolo/internal/adapter/gorm"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
	"github.com/pkg/errors"
)

// TestPersonalVirtualModelStore_RenameCollisionRejected pins the translation
// of a backend unique violation on idx_pvm_user_name into
// port.ErrAlreadyExists at SavePersonalVirtualModel time. The previous
// incarnation used errors.Is(err, gorm.ErrDuplicatedKey), which is never
// produced because db.Config.TranslateError is not set anywhere; the current
// mapping uses isUniqueViolation (dialect.go) and inspects *sqlite3.Error /
// *pgconn.PgError directly. The two supported backends spell the offending
// index differently (SQLite reports "table.user_id, table.name", PostgreSQL
// the index name "idx_pvm_user_name"), so this must hold on both. Run it
// with XOLO_TEST_POSTGRES_DSN set to cover the PostgreSQL side.
func TestPersonalVirtualModelStore_RenameCollisionRejected(t *testing.T) {
	eachBackend(t, scenarioPersonalVirtualModelStoreRenameCollisionRejected)
}

func scenarioPersonalVirtualModelStoreRenameCollisionRejected(t *testing.T, store *xologorm.Store) {
	ctx := context.Background()

	first := model.NewPersonalVirtualModel(model.UserID("rename-test-user-1"), "assistant", "Personal assistant")
	if err := store.CreatePersonalVirtualModel(ctx, first); err != nil {
		t.Fatalf("CreatePersonalVirtualModel (first): %v", err)
	}

	second := model.NewPersonalVirtualModel(model.UserID("rename-test-user-1"), "expert", "Domain expert")
	if err := store.CreatePersonalVirtualModel(ctx, second); err != nil {
		t.Fatalf("CreatePersonalVirtualModel (second): %v", err)
	}

	// Rename `second` onto the unique name held by `first`. The
	// (user_id, name) unique index must reject, and the store must
	// surface it as port.ErrAlreadyExists so the webui update handler
	// can redirect to ?error=exists.
	second.SetName("assistant")
	err := store.SavePersonalVirtualModel(ctx, second)
	if !errors.Is(err, port.ErrAlreadyExists) {
		t.Fatalf("SavePersonalVirtualModel (rename collision): expected port.ErrAlreadyExists, got %v", err)
	}
}
