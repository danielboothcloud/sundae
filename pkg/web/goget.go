package web

import (
	"net/http"
	"net/url"
	"path"
	"text/template"

	"charm.land/log/v2"
	"github.com/danielboothcloud/sundae/pkg/access"
	"github.com/danielboothcloud/sundae/pkg/backend"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
	"github.com/danielboothcloud/sundae/pkg/utils"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var goGetCounter = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "sundae",
	Subsystem: "http",
	Name:      "go_get_total",
	Help:      "The total number of go get requests",
}, []string{"repo"})

var repoIndexHTMLTpl = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta http-equiv="Content-Type" content="text/html; charset=utf-8"/>
    <meta http-equiv="refresh" content="0; url=https://godoc.org/{{ .ImportRoot }}/{{.Repo}}">
    <meta name="go-import" content="{{ .ImportRoot }}/{{ .Repo }} git {{ .Config.HTTP.PublicURL }}/{{ .Repo }}.git">
</head>
<body>
Redirecting to docs at <a href="https://godoc.org/{{ .ImportRoot }}/{{ .Repo }}">godoc.org/{{ .ImportRoot }}/{{ .Repo }}</a>...
{{- if .CloneCmd }}
<p>Clone: <code>{{ .CloneCmd }}</code></p>
{{- end }}
</body>
</html>
`))

// GoGetHandler handles go get requests.
func GoGetHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := config.FromContext(ctx)
	be := backend.FromContext(ctx)
	logger := log.FromContext(ctx)
	repo := mux.Vars(r)["repo"]

	// Handle go get requests.
	//
	// Walk up the path to the closest repository the caller can read, or
	// return a 404 if there is none.
	//
	// https://golang.org/cmd/go/#hdr-Remote_import_paths
	// https://go.dev/ref/mod#vcs-branch
	if r.URL.Query().Get("go-get") == "1" {
		repo := repo
		importRoot, err := url.Parse(cfg.HTTP.PublicURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Find the closest repository the caller can read; skip the rest.
		user := proto.UserFromContext(ctx)
		var found proto.Repository
		for {
			if rr, err := be.Repository(ctx, repo); err == nil && be.AccessLevelForUser(ctx, repo, user) >= access.ReadOnlyAccess {
				found = rr
				break
			}

			if repo == "" || repo == "." || repo == "/" {
				renderNotFound(w, r)
				return
			}

			repo = path.Dir(repo)
		}

		// The go-import tag stays "git": jj repositories are served as bare
		// git repositories. Only the human-readable clone hint differs.
		var cloneCmd string
		if found != nil && found.Vcs() == proto.VCSJJ {
			cloneCmd = proto.CloneCommand(found.Vcs(), cfg.HTTP.PublicURL+"/"+utils.SanitizeRepo(repo)+".git")
		}

		if err := repoIndexHTMLTpl.Execute(w, struct {
			Repo       string
			Config     *config.Config
			ImportRoot string
			CloneCmd   string
		}{
			Repo:       utils.SanitizeRepo(repo),
			Config:     cfg,
			ImportRoot: importRoot.Host,
			CloneCmd:   cloneCmd,
		}); err != nil {
			logger.Error("failed to render go get template", "err", err)
			renderInternalServerError(w, r)
			return
		}

		goGetCounter.WithLabelValues(repo).Inc()
		return
	}

	renderNotFound(w, r)
}
