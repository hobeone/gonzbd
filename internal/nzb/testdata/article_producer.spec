pkg ./internal/nzb/
run TestNoArticleProducedOutsideNZBPackage

[a production file outside internal/nzb constructs an Article]
file internal/app/ingest.go
--- anchor
func BuildIngestJob(cfg *config.Config, parsed *nzb.NZB,
--- replace
var _ = nzb.Article{ID: "forged@example.com", Bytes: 1, Number: 1}

func BuildIngestJob(cfg *config.Config, parsed *nzb.NZB,
--- end

[a second import name for the package does not hide the first]
file internal/app/ingest.go
--- anchor
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// BuildIngestJob converts an already-parsed NZB plus caller-supplied options
--- replace
	"github.com/hobeone/gonzbd/internal/nzb"
	zz "github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

var _ = nzb.Article{ID: "forged@example.com", Bytes: 1, Number: 1}

// BuildIngestJob converts an already-parsed NZB plus caller-supplied options
--- end
