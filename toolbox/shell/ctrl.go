/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package shell

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agent/toolbox/shell/proc"
	"go.bison9.com/agent/toolspec"
)

func (t *Tools) ctrlHandler(call *toolspec.ToolCall) (*toolspec.Result, error) {
	switch {
	case t.unattended(call):
		return refused(msgUnattendedCtrl), nil
	case !t.jobs:
		return refused(msgUnsupportedCtrl), nil
	case !t.ctrlAvailable(call):
		return refused(msgNoCtrl), nil
	}
	owner := t.owner(call)
	if owner == "" {
		return refused(msgNoOwner), nil
	}
	switch action := stringArg(call.Args, "action"); action {
	case "list":
		return textResult(listText(t.m.List(owner))), nil
	case "wait":
		return t.wait(call, owner), nil
	case "input":
		return t.input(call, owner), nil
	case "stop":
		return t.stop(call, owner), nil
	case "":
		return errResult("action is required: list, wait, input or stop"), nil
	default:
		return errResult(fmt.Sprintf("Unknown action %q: use list, wait, input or stop.", action)), nil
	}
}

// ownedJob returns the caller's job named by the id argument. Another
// owner's job is unknown, exactly like one that does not exist.
func (t *Tools) ownedJob(call *toolspec.ToolCall, owner string) (proc.Info, *toolspec.Result) {
	id := strings.TrimSpace(stringArg(call.Args, "id"))
	if id == "" {
		return proc.Info{}, errResult("id is required")
	}
	info, ok := t.m.Get(id)
	if !ok || info.Owner != owner {
		return proc.Info{}, errResult(fmt.Sprintf("Unknown job id: %s.", id))
	}
	return info, nil
}

func (t *Tools) wait(call *toolspec.ToolCall, owner string) *toolspec.Result {
	info, res := t.ownedJob(call, owner)
	if res != nil {
		return res
	}
	var match *regexp.Regexp
	if pattern := stringArg(call.Args, "until"); pattern != "" {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return errResult(fmt.Sprintf("until is not a valid regular expression: %v", err))
		}
		match = re
	}
	ctx := callContext(call)
	wait := budget(ctx, t.timeoutArg(call.Args, "seconds", DefaultWaitSeconds*time.Second))
	r, err := t.m.Wait(ctx, info.ID, proc.Until{Match: match, Timeout: wait, MaxOutput: resultLimit})
	if err != nil && r.Reason == "" {
		return errResult("could not wait for the job: " + err.Error())
	}
	return textResult(waitText(r, wait))
}

func (t *Tools) input(call *toolspec.ToolCall, owner string) *toolspec.Result {
	info, res := t.ownedJob(call, owner)
	if res != nil {
		return res
	}
	text, ok := call.Args["text"].(string)
	if !ok {
		return errResult("text is required")
	}
	if !boolArg(call.Args, "raw", false) {
		text += "\n"
	}
	err := t.m.Write(info.ID, []byte(text))
	switch {
	case errors.Is(err, proc.ErrEnded):
		return errResult(fmt.Sprintf("Job %s has ended; nothing was sent.", info.ID))
	case errors.Is(err, proc.ErrNoInput):
		return errResult(fmt.Sprintf("Job %s does not take input: only jobs started with background do.", info.ID))
	case err != nil:
		return errResult(fmt.Sprintf("Could not send input to job %s: %v.", info.ID, err))
	}
	t.audit(AuditEvent{Kind: AuditInput, Call: call, Owner: owner, JobID: info.ID, Command: info.Command, Bytes: len(text), Route: info.Route})
	return textResult(fmt.Sprintf("Sent %d bytes to job %s.", len(text), info.ID))
}

func (t *Tools) stop(call *toolspec.ToolCall, owner string) *toolspec.Result {
	if boolArg(call.Args, "all", false) {
		return t.stopAll(call, owner)
	}
	info, res := t.ownedJob(call, owner)
	if res != nil {
		return res
	}
	if !info.Running() {
		return textResult(fmt.Sprintf("Job %s had already ended: %s.", info.ID, stateText(info)))
	}
	t.stopJob(call, owner, info)
	got, _ := t.m.Get(info.ID)
	return textResult(fmt.Sprintf("Stopped job %s after %s.\nOutput file: %s", info.ID, human(got.Ended.Sub(got.Started)), got.OutputFile))
}

func (t *Tools) stopAll(call *toolspec.ToolCall, owner string) *toolspec.Result {
	running := t.m.Running(owner)
	if len(running) == 0 {
		return textResult("No running jobs.")
	}
	var wg sync.WaitGroup
	ids := make([]string, len(running))
	for i, info := range running {
		ids[i] = info.ID
		wg.Go(func() { t.stopJob(call, owner, info) })
	}
	wg.Wait()
	return textResult(fmt.Sprintf("Stopped %d %s: %s.", len(ids), plural(len(ids), "job", "jobs"), strings.Join(ids, ", ")))
}

// stopJob stops a job the agent asked to stop. The agent knows it ended, so
// no end-of-job notice is sent.
func (t *Tools) stopJob(call *toolspec.ToolCall, owner string, info proc.Info) {
	if err := t.m.SetNotify(info.ID, false); err != nil && !errors.Is(err, proc.ErrEnded) {
		t.log.Warn("shell job notify not cleared", "job", info.ID, "error", err)
	}
	if err := t.m.Stop(info.ID, t.grace); err != nil {
		t.log.Warn("shell job not stopped", "job", info.ID, "error", err)
		return
	}
	t.audit(AuditEvent{Kind: AuditStop, Call: call, Owner: owner, JobID: info.ID, Command: info.Command, Route: info.Route})
}
