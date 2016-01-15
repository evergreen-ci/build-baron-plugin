package buildbaron

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/10gen-labs/slogger/v1"
	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/model"
	"github.com/evergreen-ci/evergreen/plugin"
	"github.com/evergreen-ci/evergreen/thirdparty"
	"net/http"
	"strings"
	"text/template"
)

const FailingTasksField = "customfield_12950"

const UIRoot = "https://evergreen.mongodb.com"

const DescriptionTemplateString = `
h2. [{{.Task.DisplayName}} failed on {{.Task.BuildVariant}}|` + UIRoot + `/task/{{.Task.Id}}]

{{range .Tests}}*{{.Name}}* - [Logs|{{.URL}}] | [History|{{.HistoryURL}}]

{{end}}



~BF Ticket Generated by [~{{.UserId}}]~
`

var DescriptionTemplate = template.Must(template.New("Desc").Parse(DescriptionTemplateString))

// jiraTestFailure contains the required fields for generating a failure report.
type jiraTestFailure struct {
	Name       string
	URL        string
	HistoryURL string
}

// fileTicket creates a JIRA ticket for a task with the given test failures.
func (bbp *BuildBaronPlugin) fileTicket(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TaskId  string   `json:"task"`
		TestIds []string `json:"tests"`
	}
	json.NewDecoder(r.Body).Decode(&input)

	// grab the task and user info to fill out the ticket
	u := plugin.GetUser(r)
	if u == nil {
		plugin.WriteJSON(w, http.StatusUnauthorized, "must be logged in to file a ticket")
		return
	}
	t, err := model.FindTask(input.TaskId)
	if err != nil {
		plugin.WriteJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if t == nil {
		plugin.WriteJSON(w, http.StatusNotFound, fmt.Sprintf("task not found for id %v", input.TaskId))
		return
	}

	// build a list of all failed tests to include
	testIds := map[string]bool{}
	for _, testId := range input.TestIds {
		testIds[testId] = true
	}
	tests := []jiraTestFailure{}
	for _, test := range t.TestResults {
		if testIds[test.TestFile] {
			tests = append(tests, jiraTestFailure{
				Name:       cleanTestName(test.TestFile),
				URL:        test.URL,
				HistoryURL: historyURL(t, cleanTestName(test.TestFile)),
			})
		}
	}

	//lay out the JIRA API request
	request := map[string]interface{}{}
	request["project"] = map[string]string{"key": "BF"}
	request["summary"] = getSummary(t.DisplayName, tests)
	request[FailingTasksField] = []string{t.DisplayName}
	request["issuetype"] = map[string]string{"name": "Build Failure"}
	request["assignee"] = map[string]string{"name": u.Id}
	request["reporter"] = map[string]string{"name": u.Id}
	request["description"], err = getDescription(t, u.Id, tests)
	if err != nil {
		plugin.WriteJSON(
			w, http.StatusBadRequest, fmt.Sprintf("error creating description: %v", err))
		return
	}

	evergreen.Logger.Logf(slogger.INFO, fmt.Sprintf("Creating JIRA ticket for user %v", u.Id))

	jiraHandler := thirdparty.NewJiraHandler(
		bbp.opts.Host,
		bbp.opts.Username,
		bbp.opts.Password,
	)
	result, err := jiraHandler.CreateTicket(request)
	if err != nil {
		msg := fmt.Sprintf("error creating JIRA ticket: %v", err)
		evergreen.Logger.Logf(slogger.ERROR, msg)
		plugin.WriteJSON(w, http.StatusBadRequest, msg)
		return
	}
	evergreen.Logger.Logf(slogger.INFO, fmt.Sprintf("Ticket %v successfully created", result.Key))
	plugin.WriteJSON(w, http.StatusOK, result)
}

func cleanTestName(path string) string {
	if unixIdx := strings.LastIndex(path, "/"); unixIdx != -1 {
		// if the path ends in a slash, remove it and try again
		if unixIdx == len(path)-1 {
			return cleanTestName(path[:len(path)-1])
		}
		return path[unixIdx+1:]
	}
	if windowsIdx := strings.LastIndex(path, `\`); windowsIdx != -1 {
		// if the path ends in a slash, remove it and try again
		if windowsIdx == len(path)-1 {
			return cleanTestName(path[:len(path)-1])
		}
		return path[windowsIdx+1:]
	}
	return path
}

func historyURL(t *model.Task, testName string) string {
	return fmt.Sprintf("%v/task_history/%v/%v#%v=fail",
		UIRoot, t.Project, t.DisplayName, testName)
}

func getSummary(taskName string, tests []jiraTestFailure) string {
	switch {
	case len(tests) == 0:
		// this is likely a compile failure
		return fmt.Sprintf("%v failure", taskName)
	case len(tests) > 4:
		// if there are many failures, just squish the summary
		return fmt.Sprintf("%v failures", taskName)
	default:
		names := []string{}
		for _, t := range tests {
			names = append(names, t.Name)
		}
		return strings.Join(names, ", ")
	}
}

func getDescription(t *model.Task, userId string, tests []jiraTestFailure) (string, error) {
	args := struct {
		Task   *model.Task
		UserId string
		Tests  []jiraTestFailure
	}{t, userId, tests}
	buf := &bytes.Buffer{}
	if err := DescriptionTemplate.Execute(buf, args); err != nil {
		return "", err
	}
	return buf.String(), nil
}
