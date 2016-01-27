package buildbaron

import (
	"fmt"
	"github.com/10gen-labs/slogger/v1"
	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/db"
	"github.com/evergreen-ci/evergreen/db/bsonutil"
	"github.com/evergreen-ci/evergreen/model"
	"github.com/evergreen-ci/evergreen/plugin"
	"github.com/evergreen-ci/evergreen/thirdparty"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/gorilla/mux"
	"github.com/mitchellh/mapstructure"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"html/template"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"
)

func init() {
	plugin.Publish(&BuildBaronPlugin{})
}

const (
	PluginName  = "buildbaron"
	JIRAFailure = "Error searching jira for ticket"
	JQLBFQuery  = "(project in (BF, SERVER, WT)) and ( %v ) order by resolution desc, status asc, resolutionDate desc, updatedDate desc"

	NotesCollection = "build_baron_notes"
	msPerNS         = 1000 * 1000
	maxNoteSize     = 16 * 1024 // 16KB
)

type bbPluginOptions struct {
	Host     string
	Username string
	Password string
	Projects []string
}

type BuildBaronPlugin struct {
	opts *bbPluginOptions
}

// A regex that matches either / or \ for splitting directory paths
// on either windows or linux paths.
var eitherSlash *regexp.Regexp = regexp.MustCompile(`[/\\]`)

func (bbp *BuildBaronPlugin) Name() string {
	return PluginName
}

// GetUIHandler adds a path for looking up build failures in JIRA.
func (bbp *BuildBaronPlugin) GetUIHandler() http.Handler {
	if bbp.opts == nil {
		panic("build baron plugin missing configuration")
	}
	r := mux.NewRouter()
	r.Path("/jira_bf_search/{task_id}").HandlerFunc(bbp.buildFailuresSearch)
	r.Path("/note/{task_id}").Methods("GET").HandlerFunc(bbp.getNote)
	r.Path("/note/{task_id}").Methods("PUT").HandlerFunc(bbp.saveNote)
	r.Path("/file_ticket").Methods("POST").HandlerFunc(bbp.fileTicket)
	return r
}

func (bbp *BuildBaronPlugin) Configure(conf map[string]interface{}) error {
	// pull out options needed from config file (JIRA authentication info, and list of projects)
	bbpOptions := &bbPluginOptions{}
	err := mapstructure.Decode(conf, bbpOptions)
	if err != nil {
		return err
	}
	if bbpOptions.Host == "" || bbpOptions.Username == "" || bbpOptions.Password == "" {
		return fmt.Errorf("Host, username, and password in config must not be blank")
	}
	bbp.opts = bbpOptions
	return nil
}

func (bbp *BuildBaronPlugin) GetPanelConfig() (*plugin.PanelConfig, error) {
	root := plugin.StaticWebRootFromSourceFile()
	panelHTML, err := ioutil.ReadFile(root + "/partials/ng_include_task_build_baron.html")
	if err != nil {
		return nil, fmt.Errorf("Can't load panel html file, %v, %v",
			root+"/partials/ng_include_task_build_baron.html", err)
	}

	includeJS, err := ioutil.ReadFile(root + "/partials/script_task_build_baron_js.html")
	if err != nil {
		return nil, fmt.Errorf("Can't load panel html file, %v, %v",
			root+"/partials/script_task_build_baron_js.html", err)
	}

	includeCSS, err := ioutil.ReadFile(root +
		"/partials/link_task_build_baron_css.html")
	if err != nil {
		return nil, fmt.Errorf("Can't load panel html file, %v, %v",
			root+"/partials/link_task_build_baron_css.html", err)
	}

	return &plugin.PanelConfig{
		StaticRoot: plugin.StaticWebRootFromSourceFile(),
		Panels: []plugin.UIPanel{
			{
				Page:      plugin.TaskPage,
				Position:  plugin.PageRight,
				PanelHTML: template.HTML(panelHTML),
				Includes:  []template.HTML{template.HTML(includeCSS), template.HTML(includeJS)},
				DataFunc: func(context plugin.UIContext) (interface{}, error) {
					return struct {
						Enabled bool `json:"enabled"`
					}{util.SliceContains(bbp.opts.Projects, context.ProjectRef.Identifier)}, nil
				},
			},
		},
	}, nil
}

// BuildFailuresSearchHandler handles the requests of searching jira in the build
//  failures project
func (bbp *BuildBaronPlugin) buildFailuresSearch(w http.ResponseWriter, r *http.Request) {
	taskId := mux.Vars(r)["task_id"]
	task, err := model.FindTask(taskId)
	if err != nil {
		plugin.WriteJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	jql := taskToJQL(task)

	jiraHandler := thirdparty.NewJiraHandler(
		bbp.opts.Host,
		bbp.opts.Username,
		bbp.opts.Password,
	)

	results, err := jiraHandler.JQLSearch(jql)
	if err != nil {
		message := fmt.Sprintf("%v: %v, %v", JIRAFailure, err, jql)
		evergreen.Logger.Errorf(slogger.ERROR, message)
		plugin.WriteJSON(w, http.StatusInternalServerError, message)
		return
	}
	plugin.WriteJSON(w, http.StatusOK, results)
}

// getNote retrieves the latest note from the database.
func (bbp *BuildBaronPlugin) getNote(w http.ResponseWriter, r *http.Request) {
	taskId := mux.Vars(r)["task_id"]
	n, err := NoteForTask(taskId)
	if err != nil {
		plugin.WriteJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == nil {
		plugin.WriteJSON(w, http.StatusOK, "")
		return
	}
	plugin.WriteJSON(w, http.StatusOK, n)
}

// saveNote reads a request containing a note's content along with the last seen
// edit time and updates the note in the database.
func (bbp *BuildBaronPlugin) saveNote(w http.ResponseWriter, r *http.Request) {
	taskId := mux.Vars(r)["task_id"]
	n := &Note{}
	if err := util.ReadJSONInto(r.Body, n); err != nil {
		plugin.WriteJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	// prevent incredibly large notes
	if len(n.Content) > maxNoteSize {
		plugin.WriteJSON(w, http.StatusBadRequest, "note is too large")
		return
	}

	// We need to make sure the user isn't blowing away a new edit,
	// so we load the existing note. If the user's last seen edit time is less
	// than the most recent edit, we error with a helpful message.
	old, err := NoteForTask(taskId)
	if err != nil {
		plugin.WriteJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// we compare times by millisecond rather than nanosecond so we can
	// work around the rounding that occurs when javascript forces these
	// large values into in float type.
	if old != nil && n.UnixNanoTime/msPerNS != old.UnixNanoTime/msPerNS {
		plugin.WriteJSON(w, http.StatusBadRequest,
			"this note has already been edited. Please refresh and try again.")
		return
	}

	n.TaskId = taskId
	n.UnixNanoTime = time.Now().UnixNano()
	if err := n.Upsert(); err != nil {
		plugin.WriteJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	plugin.WriteJSON(w, http.StatusOK, n)
}

// In order that we can write tests without an actual jira server handy
type jqlSearcher interface {
	JQLSearch(query string) (*thirdparty.JiraSearchResults, error)
}

// Generates a jira JQL string from the task
// When we search in jira for a task we search in the BF package
// If there are any test results, then we only search by test file
// name of all of the failed tests.
// Otherwise we search by the task name.
func taskToJQL(task *model.Task) string {
	var jqlParts []string
	var jqlClause string
	for _, testResult := range task.TestResults {
		if testResult.Status == evergreen.TestFailedStatus {
			fileParts := eitherSlash.Split(testResult.TestFile, -1)
			jqlParts = append(jqlParts, fmt.Sprintf("text~\"%v\"", fileParts[len(fileParts)-1]))
		}
	}
	if jqlParts != nil {
		jqlClause = strings.Join(jqlParts, " or ")
	} else {
		jqlClause = fmt.Sprintf("text~\"%v\"", task.DisplayName)
	}

	return fmt.Sprintf(JQLBFQuery, jqlClause)
}

// Note contains arbitrary information entered by an Evergreen user, scope to a task.
type Note struct {
	TaskId       string `bson:"_id" json:"-"`
	UnixNanoTime int64  `bson:"time" json:"time"`
	Content      string `bson:"content" json:"content"`
}

// Note DB Logic

var TaskIdKey = bsonutil.MustHaveTag(Note{}, "TaskId")

// Upsert overwrites an existing note.
func (n *Note) Upsert() error {
	_, err := db.Upsert(
		NotesCollection,
		bson.D{{TaskIdKey, n.TaskId}},
		n,
	)
	return err
}

// NoteForTask returns the note for the given task Id, if it exists.
func NoteForTask(taskId string) (*Note, error) {
	n := &Note{}
	err := db.FindOneQ(
		NotesCollection,
		db.Query(bson.D{{TaskIdKey, taskId}}),
		n,
	)
	if err == mgo.ErrNotFound {
		return nil, nil
	}
	return n, err
}
