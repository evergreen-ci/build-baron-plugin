package buildbaron

import (
	"fmt"
	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/db"
	"github.com/evergreen-ci/evergreen/model"
	. "github.com/smartystreets/goconvey/convey"
	"testing"
	"time"
)

func init() {
	db.SetGlobalSessionProvider(db.SessionFactoryFromConfig(evergreen.TestConfig()))
}

const (
	jiraFailure = "fake jira failed"
)

func TestTaskToJQL(t *testing.T) {
	Convey("Given a task with with two failed tests and one successful test, "+
		"the jql should contian only the failed test names", t, func() {
		task1 := model.Task{}
		task1.TestResults = []model.TestResult{
			{Status: "fail", TestFile: "foo.js"},
			{Status: "success", TestFile: "bar.js"},
			{Status: "fail", TestFile: "baz.js"},
		}
		task1.DisplayName = "foobar"
		jQL1 := taskToJQL(&task1)
		referenceJQL1 := fmt.Sprintf(JQLBFQuery, "text~\"foo.js\" or text~\"baz.js\"")
		So(jQL1, ShouldEqual, referenceJQL1)
	})

	Convey("Given a task with with oo failed tests, "+
		"the jql should contian only the failed task name", t, func() {
		task2 := model.Task{}
		task2.TestResults = []model.TestResult{}
		task2.DisplayName = "foobar"
		jQL2 := taskToJQL(&task2)
		referenceJQL2 := fmt.Sprintf(JQLBFQuery, "text~\"foobar\"")
		So(jQL2, ShouldEqual, referenceJQL2)
	})
}

func TestNoteStorage(t *testing.T) {
	Convey("With a test note to save", t, func() {
		db.Clear(NotesCollection)
		n := Note{
			TaskId:       "t1",
			UnixNanoTime: time.Now().UnixNano(),
			Content:      "test note",
		}
		Convey("saving the note should work without error", func() {
			So(n.Upsert(), ShouldBeNil)

			Convey("the note should be retrievable", func() {
				n2, err := NoteForTask("t1")
				So(err, ShouldBeNil)
				So(n2, ShouldNotBeNil)
				So(*n2, ShouldResemble, n)
			})
			Convey("saving the note again should overwrite the existing note", func() {
				n3 := n
				n3.Content = "new content"
				So(n3.Upsert(), ShouldBeNil)
				n4, err := NoteForTask("t1")
				So(err, ShouldBeNil)
				So(n4, ShouldNotBeNil)
				So(n4.TaskId, ShouldEqual, "t1")
				So(n4.Content, ShouldEqual, n3.Content)
			})
		})
	})
}
