package backends

import (
	"github.com/sniperkit/pivot/dal"
)

type Migratable interface {
	Migrate(diff []dal.SchemaDelta) error
}
