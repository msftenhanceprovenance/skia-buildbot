package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/gorilla/mux"
	"go.skia.org/infra/am/go/incident"
	"go.skia.org/infra/am/go/note"
	"go.skia.org/infra/am/go/silence"
	"go.skia.org/infra/go/alerts"
	"go.skia.org/infra/go/allowed"
	"go.skia.org/infra/go/auditlog"
	"go.skia.org/infra/go/auth"
	"go.skia.org/infra/go/baseapp"
	"go.skia.org/infra/go/ds"
	"go.skia.org/infra/go/httputils"
	"go.skia.org/infra/go/login"
	"go.skia.org/infra/go/metrics2"
	"go.skia.org/infra/go/sklog"
	"go.skia.org/infra/go/util"
	"google.golang.org/api/option"
	secure "gopkg.in/unrolled/secure.v1"
)

// flags
var (
	assignGroup        = flag.String("assign_group", "google/skia-root@google.com", "The chrome infra auth group to use for users incidents can be assigned to.")
	authGroup          = flag.String("auth_group", "google/skia-staff@google.com", "The chrome infra auth group to use for restricting access.")
	chromeInfraAuthJWT = flag.String("chrome_infra_auth_jwt", "/var/secrets/skia-public-auth/key.json", "The JWT key for the service account that has access to chrome infra auth.")
	namespace          = flag.String("namespace", "", "The Cloud Datastore namespace, such as 'perf'.")
	internalPort       = flag.String("internal_port", ":9000", "HTTP internal service address (e.g., ':9000') for unauthenticated in-cluster requests.")
	project            = flag.String("project", "skia-public", "The Google Cloud project name.")
)

const (
	// EXPIRE_DURATION is the time to wait before expiring an incident.
	EXPIRE_DURATION = 2 * time.Minute
)

// Server is the state of the server.
type Server struct {
	incidentStore *incident.Store
	silenceStore  *silence.Store
	templates     *template.Template
	allow         allowed.Allow // Who is allowed to use the site.
	assign        allowed.Allow // A list of people that incidents can be assigned to.
}

// See baseapp.Constructor.
func New() (baseapp.App, error) {
	var allow allowed.Allow
	var assign allowed.Allow
	if !*baseapp.Local {
		ts, err := auth.NewJWTServiceAccountTokenSource("", *chromeInfraAuthJWT, auth.SCOPE_USERINFO_EMAIL)
		if err != nil {
			return nil, err
		}
		client := httputils.DefaultClientConfig().WithTokenSource(ts).With2xxOnly().Client()
		allow, err = allowed.NewAllowedFromChromeInfraAuth(client, *authGroup)
		if err != nil {
			return nil, err
		}
		assign, err = allowed.NewAllowedFromChromeInfraAuth(client, *assignGroup)
		if err != nil {
			return nil, err
		}
	} else {
		allow = allowed.NewAllowedFromList([]string{"fred@example.org", "barney@example.org", "wilma@example.org"})
		assign = allowed.NewAllowedFromList([]string{"betty@example.org", "fred@example.org", "barney@example.org", "wilma@example.org"})
	}

	login.InitWithAllow(*baseapp.Port, *baseapp.Local, nil, nil, allow)

	ctx := context.Background()
	ts, err := auth.NewDefaultTokenSource(*baseapp.Local, pubsub.ScopePubSub, "https://www.googleapis.com/auth/datastore")
	if err != nil {
		return nil, err
	}

	if *namespace == "" {
		return nil, fmt.Errorf("The --namespace flag is required. See infra/DATASTORE.md for format details.\n")
	}
	if !*baseapp.Local && !util.In(*namespace, []string{ds.ALERT_MANAGER_NS}) {
		return nil, fmt.Errorf("When running in prod the datastore namespace must be a known value.")
	}
	if err := ds.InitWithOpt(*project, *namespace, option.WithTokenSource(ts)); err != nil {
		return nil, fmt.Errorf("Failed to init Cloud Datastore: %s", err)
	}

	client, err := pubsub.NewClient(ctx, *project, option.WithTokenSource(ts))
	if err != nil {
		return nil, err
	}
	topic := client.Topic(alerts.TOPIC)
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}

	// When running in production we have every instance use the same topic name so that
	// they load-balance pulling items from the topic.
	subName := fmt.Sprintf("%s-%s", alerts.TOPIC, "prod")
	if *baseapp.Local {
		// When running locally create a new topic for every host.
		subName = fmt.Sprintf("%s-%s", alerts.TOPIC, hostname)
	}
	sub := client.Subscription(subName)
	ok, err := sub.Exists(ctx)
	if err != nil {
		return nil, fmt.Errorf("Failed checking subscription existence: %s", err)
	}
	if !ok {
		sub, err = client.CreateSubscription(ctx, subName, pubsub.SubscriptionConfig{
			Topic: topic,
		})
		if err != nil {
			return nil, fmt.Errorf("Failed creating subscription: %s", err)
		}
	}

	srv := &Server{
		incidentStore: incident.NewStore(ds.DS, []string{"kubernetes_pod_name", "instance", "pod_template_hash"}),
		silenceStore:  silence.NewStore(ds.DS),
		allow:         allow,
		assign:        assign,
	}
	srv.loadTemplates()

	locations := []string{"skia-public", "google.com:skia-corp", "google.com:skia-buildbots"}
	livenesses := map[string]metrics2.Liveness{}
	for _, location := range locations {
		livenesses[location] = metrics2.NewLiveness("alive", map[string]string{"location": location})
	}

	// Process all incoming PubSub requests.
	go func() {
		for {
			err := sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
				msg.Ack()
				var m map[string]string
				if err := json.Unmarshal(msg.Data, &m); err != nil {
					sklog.Error(err)
					return
				}
				if m[alerts.TYPE] == alerts.TYPE_HEALTHZ {
					sklog.Infof("healthz received: %q", m[alerts.LOCATION])
					if l, ok := livenesses[m[alerts.LOCATION]]; ok {
						l.Reset()
					} else {
						sklog.Errorf("Unknown PubSub source location: %q", m[alerts.LOCATION])
					}
				} else {
					if _, err := srv.incidentStore.AlertArrival(m); err != nil {
						sklog.Errorf("Error processing alert: %s", err)
					}
				}
			})
			if err != nil {
				sklog.Errorf("Failed receiving pubsub message: %s", err)
			}
		}
	}()

	// This is really just a backstop in case we miss a resolved event for the incident.
	go func() {
		for _ = range time.Tick(1 * time.Minute) {
			ins, err := srv.incidentStore.GetAll()
			if err != nil {
				sklog.Errorf("Failed to load incidents: %s", err)
				continue
			}
			now := time.Now()
			for _, in := range ins {
				// If it was last updated too long ago then it should be archived.
				if time.Unix(in.LastSeen, 0).Add(EXPIRE_DURATION).Before(now) {
					if _, err := srv.incidentStore.Archive(in.Key); err != nil {
						sklog.Errorf("Failed to archive incident: %s", err)
					}
				}
			}
		}
	}()

	srv.startInternalServer()

	return srv, nil
}

func (srv *Server) loadTemplates() {
	srv.templates = template.Must(template.New("").Delims("{%", "%}").ParseFiles(
		filepath.Join(*baseapp.ResourcesDir, "index.html"),
	))
}

func (srv *Server) mainHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	if *baseapp.Local {
		srv.loadTemplates()
	}
	if err := srv.templates.ExecuteTemplate(w, "index.html", map[string]string{
		// Look in webpack.config.js for where the nonce templates are injected.
		"nonce": secure.CSPNonce(r.Context()),
	}); err != nil {
		sklog.Errorf("Failed to expand template: %s", err)
	}
}

type AddNoteRequest struct {
	Text string `json:"text"`
	Key  string `json:"key"`
}

// user returns the currently logged in user, or a placeholder if running locally.
func (srv *Server) user(r *http.Request) string {
	user := "barney@example.org"
	if !*baseapp.Local {
		user = login.LoggedInAs(r)
	}
	return user
}

func (srv *Server) addNoteHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req AddNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputils.ReportError(w, r, err, "Failed to decode add note request.")
		return
	}
	auditlog.Log(r, "add-note", req)

	note := note.Note{
		Text:   req.Text,
		TS:     time.Now().Unix(),
		Author: srv.user(r),
	}
	in, err := srv.incidentStore.AddNote(req.Key, note)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to add note.")
		return
	}
	if err := json.NewEncoder(w).Encode(in); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

func (srv *Server) addSilenceNoteHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req AddNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputils.ReportError(w, r, err, "Failed to decode add note request.")
		return
	}
	auditlog.Log(r, "add-silence-note", req)

	note := note.Note{
		Text:   req.Text,
		TS:     time.Now().Unix(),
		Author: srv.user(r),
	}
	in, err := srv.silenceStore.AddNote(req.Key, note)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to add note.")
		return
	}
	if err := json.NewEncoder(w).Encode(in); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

type DelNoteRequest struct {
	Index int    `json:"index"`
	Key   string `json:"key"`
}

func (srv *Server) delNoteHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req DelNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputils.ReportError(w, r, err, "Failed to decode add note request.")
		return
	}
	auditlog.Log(r, "del-note", req)
	in, err := srv.incidentStore.DeleteNote(req.Key, req.Index)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to add note.")
		return
	}
	if err := json.NewEncoder(w).Encode(in); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

func (srv *Server) delSilenceNoteHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req DelNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputils.ReportError(w, r, err, "Failed to decode add note request.")
		return
	}
	auditlog.Log(r, "del-silence-note", req)
	in, err := srv.silenceStore.DeleteNote(req.Key, req.Index)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to add note.")
		return
	}
	if err := json.NewEncoder(w).Encode(in); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

type TakeRequest struct {
	Key string `json:"key"`
}

func (srv *Server) takeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req TakeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputils.ReportError(w, r, err, "Failed to decode take request.")
		return
	}
	auditlog.Log(r, "take", req)

	in, err := srv.incidentStore.Assign(req.Key, srv.user(r))
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to assign.")
		return
	}
	if err := json.NewEncoder(w).Encode(in); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

type StatsRequest struct {
	Range string `json:"range"`
}

type Stat struct {
	Num      int               `json:"num"`
	Incident incident.Incident `json:"incident"`
}

type StatsResponse []*Stat

type StatsResponseSlice StatsResponse

func (p StatsResponseSlice) Len() int           { return len(p) }
func (p StatsResponseSlice) Less(i, j int) bool { return p[i].Num > p[j].Num }
func (p StatsResponseSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

func (srv *Server) statsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req StatsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputils.ReportError(w, r, err, "Failed to decode stats request.")
		return
	}
	ins, err := srv.incidentStore.GetRecentlyResolvedInRange(req.Range)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to query for Incidents.")
	}
	count := map[string]*Stat{}
	for _, in := range ins {
		if stat, ok := count[in.ID]; !ok {
			count[in.ID] = &Stat{
				Num:      1,
				Incident: in,
			}
		} else {
			stat.Num += 1
		}
	}
	ret := StatsResponse{}
	for _, v := range count {
		ret = append(ret, v)
	}
	sort.Sort(StatsResponseSlice(ret))
	if err := json.NewEncoder(w).Encode(ret); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

type IncidentsInRangeRequest struct {
	Range    string            `json:"range"`
	Incident incident.Incident `json:"incident"`
}

func (srv *Server) incidentsInRangeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req IncidentsInRangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputils.ReportError(w, r, err, "Failed to decode incident range request.")
		return
	}
	ret, err := srv.incidentStore.GetRecentlyResolvedInRangeWithID(req.Range, req.Incident.ID)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to query for incidents.")
	}
	if err := json.NewEncoder(w).Encode(ret); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

type AssignRequest struct {
	Key   string `json:"key"`
	Email string `json:"email"`
}

func (srv *Server) assignHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req AssignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputils.ReportError(w, r, err, "Failed to decode take request.")
		return
	}
	auditlog.Log(r, "assign", req)
	in, err := srv.incidentStore.Assign(req.Key, req.Email)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to assign.")
		return
	}
	if err := json.NewEncoder(w).Encode(in); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

func (srv *Server) emailsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	emails := srv.assign.Emails()
	sort.Strings(emails)
	if err := json.NewEncoder(w).Encode(&emails); err != nil {
		sklog.Errorf("Failed to encode emails: %s", err)
	}
}

func (srv *Server) silencesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	silences, err := srv.silenceStore.GetAll()
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to load recents.")
		return
	}
	if silences == nil {
		silences = []silence.Silence{}
	}
	recents, err := srv.silenceStore.GetRecentlyArchived()
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to load recents.")
		return
	}
	silences = append(silences, recents...)
	if err := json.NewEncoder(w).Encode(silences); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

func (srv *Server) incidentHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ins, err := srv.incidentStore.GetAll()
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to load incidents.")
		return
	}
	recents, err := srv.incidentStore.GetRecentlyResolved()
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to load recents.")
		return
	}
	ins = append(ins, recents...)
	if err := json.NewEncoder(w).Encode(ins); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

func (srv *Server) recentIncidentsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := r.FormValue("id")
	key := r.FormValue("key")
	ins, err := srv.incidentStore.GetRecentlyResolvedForID(id, key)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to load incidents.")
		return
	}
	if err := json.NewEncoder(w).Encode(ins); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

func (srv *Server) saveSilenceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req silence.Silence
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputils.ReportError(w, r, err, "Failed to decode silence creation request.")
		return
	}
	auditlog.Log(r, "create-silence", req)
	silence, err := srv.silenceStore.Put(&req)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to create silence.")
		return
	}
	if err := json.NewEncoder(w).Encode(silence); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

func (srv *Server) archiveSilenceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req silence.Silence
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputils.ReportError(w, r, err, "Failed to decode silence creation request.")
		return
	}
	auditlog.Log(r, "archive-silence", req)
	silence, err := srv.silenceStore.Archive(req.Key)
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to archive silence.")
		return
	}
	if err := json.NewEncoder(w).Encode(silence); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

func (srv *Server) reactivateSilenceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req silence.Silence
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputils.ReportError(w, r, err, "Failed to decode silence creation request.")
		return
	}
	auditlog.Log(r, "reactivate-silence", req)
	silence, err := srv.silenceStore.Reactivate(req.Key, srv.user(r))
	if err != nil {
		httputils.ReportError(w, r, err, "Failed to archive silence.")
		return
	}
	if err := json.NewEncoder(w).Encode(silence); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

// newSilenceHandler creates and returns a new Silence pre-populated with good defaults.
func (srv *Server) newSilenceHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s := silence.New(srv.user(r))
	if err := json.NewEncoder(w).Encode(s); err != nil {
		sklog.Errorf("Failed to send response: %s", err)
	}
}

// See baseapp.App.
func (srv *Server) AddHandlers(r *mux.Router) {
	r.HandleFunc("/", srv.mainHandler)
	r.HandleFunc("/loginstatus/", login.StatusHandler).Methods("GET")

	// GETs
	r.HandleFunc("/_/emails", srv.emailsHandler).Methods("GET")
	r.HandleFunc("/_/incidents", srv.incidentHandler).Methods("GET")
	r.HandleFunc("/_/new_silence", srv.newSilenceHandler).Methods("GET")
	r.HandleFunc("/_/recent_incidents", srv.recentIncidentsHandler).Methods("GET")
	r.HandleFunc("/_/silences", srv.silencesHandler).Methods("GET")

	// POSTs
	r.HandleFunc("/_/add_note", srv.addNoteHandler).Methods("POST")
	r.HandleFunc("/_/add_silence_note", srv.addSilenceNoteHandler).Methods("POST")
	r.HandleFunc("/_/archive_silence", srv.archiveSilenceHandler).Methods("POST")
	r.HandleFunc("/_/assign", srv.assignHandler).Methods("POST")
	r.HandleFunc("/_/del_note", srv.delNoteHandler).Methods("POST")
	r.HandleFunc("/_/del_silence_note", srv.delSilenceNoteHandler).Methods("POST")
	r.HandleFunc("/_/reactivate_silence", srv.reactivateSilenceHandler).Methods("POST")
	r.HandleFunc("/_/save_silence", srv.saveSilenceHandler).Methods("POST")
	r.HandleFunc("/_/take", srv.takeHandler).Methods("POST")
	r.HandleFunc("/_/stats", srv.statsHandler).Methods("POST")
	r.HandleFunc("/_/incidents_in_range", srv.incidentsInRangeHandler).Methods("POST")
}

// See baseapp.App.
func (srv *Server) AddMiddleware() []mux.MiddlewareFunc {
	ret := []mux.MiddlewareFunc{}
	if !*baseapp.Local {
		ret = append(ret, login.ForceAuthMiddleware(login.DEFAULT_REDIRECT_URL), login.RestrictViewer)
	}
	return ret
}

func (srv *Server) startInternalServer() {
	// Internal endpoints that are only accessible from within the cluster.
	unprotected := mux.NewRouter()
	unprotected.HandleFunc("/_/incidents", srv.incidentHandler).Methods("GET")
	unprotected.HandleFunc("/_/silences", srv.silencesHandler).Methods("GET")
	go func() {
		sklog.Fatal(http.ListenAndServe(*internalPort, unprotected))
	}()
}

func main() {
	baseapp.Serve(New, []string{"am.skia.org"})
}
