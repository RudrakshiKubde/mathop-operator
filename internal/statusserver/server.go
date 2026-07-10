package statusserver

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	mathv1alpha1 "github.com/RudrakshiKubde/mathop-operator/api/v1alpha1"
	"github.com/RudrakshiKubde/mathop-operator/internal/controller"
)

type Server struct {
	Client client.Client
	Addr   string
}

func (s *Server) NeedLeaderElection() bool { return false }

func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/workflows/", s.handlePage)              // human-facing live HTML dashboard
	mux.HandleFunc("/api/workflows/stream/", s.handleStream) // SSE live updates, used by the page above
	mux.HandleFunc("/api/workflows/", s.handleWorkflowJSON)  // plain JSON snapshot, for curl/scripts

	srv := &http.Server{Addr: s.Addr, Handler: mux}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		return srv.Shutdown(context.Background())
	case err := <-errCh:
		return err
	}
}

func (s *Server) lookupWorkflow(ctx context.Context, txID string) (*mathv1alpha1.Workflow, error) {
	var list mathv1alpha1.WorkflowList
	if err := s.Client.List(ctx, &list, client.MatchingFields{controller.TransactionIndexKey: txID}); err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("not found")
	}
	return &list.Items[0], nil
}

func (s *Server) handleWorkflowJSON(w http.ResponseWriter, r *http.Request) {
	txID := strings.TrimPrefix(r.URL.Path, "/api/workflows/")
	wf, err := s.lookupWorkflow(r.Context(), txID)
	if err != nil {
		http.Error(w, "no workflow found with that transaction id", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(wf.Status)
}

// handleStream pushes the workflow's status to the browser every time it
// changes, via Server-Sent Events, until the workflow reaches a terminal
// phase — that's what makes the page update live instead of needing refresh.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	txID := strings.TrimPrefix(r.URL.Path, "/api/workflows/stream/")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	var lastPayload string
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			wf, err := s.lookupWorkflow(r.Context(), txID)
			if err != nil {
				continue
			}
			payload, _ := json.Marshal(wf.Status)
			if string(payload) != lastPayload {
				lastPayload = string(payload)
				fmt.Fprintf(w, "data: %s\n\n", payload)
				flusher.Flush()
			}
			if wf.Status.Phase == "Succeeded" || wf.Status.Phase == "Failed" {
				return
			}
		}
	}
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	txID := strings.TrimPrefix(r.URL.Path, "/workflows/")
	if txID == "" {
		http.Error(w, "usage: /workflows/{transactionID}", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	dashboardTemplate.Execute(w, map[string]string{"TxID": txID})
}

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html>
<head>
<title>Workflow {{.TxID}}</title>
<style>
body { font-family: monospace; background: #111; color: #eee; padding: 2rem; }
h1 { font-size: 1.1rem; word-break: break-all; }
.phase-banner { font-size: 1.5rem; margin: 1rem 0; }
.Succeeded { color: #4caf50; }
.Failed { color: #f44336; }
.Running { color: #ffb300; }
.Pending { color: #888; }
ul { list-style: none; padding: 0; }
li { padding: 0.5rem 0; border-bottom: 1px solid #333; }
.err { color: #f44336; margin-left: 1rem; margin-top: 0.25rem; }
</style>
</head>
<body>
<h1>Workflow {{.TxID}}</h1>
<div id="banner" class="phase-banner Pending">Loading...</div>
<ul id="tasks"></ul>
<script>
const txID = "{{.TxID}}";
const banner = document.getElementById('banner');
const list = document.getElementById('tasks');

function render(status) {
  banner.className = 'phase-banner ' + status.phase;
  banner.textContent = status.phase === 'Succeeded' ? 'COMPLETED'
    : status.phase === 'Failed' ? 'FAILED'
    : (status.phase || 'Pending');

  list.innerHTML = '';
  (status.tasks || []).forEach(t => {
    const li = document.createElement('li');
    li.className = t.phase;
    li.textContent = '[' + t.phase + '] ' + t.name;
    if (t.error) {
      const e = document.createElement('div');
      e.className = 'err';
      e.textContent = t.error;
      li.appendChild(e);
    }
    list.appendChild(li);
  });
}

fetch('/api/workflows/' + txID).then(r => r.json()).then(render);

const es = new EventSource('/api/workflows/stream/' + txID);
es.onmessage = (ev) => render(JSON.parse(ev.data));
es.onerror = () => es.close();
</script>
</body>
</html>`))
