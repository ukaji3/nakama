package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The AWS sign-in of the panel: the Login button runs what
// `nakama aws login` runs (Backend.Login). The browser goes to the AWS
// sign-in page in this tab; afterwards AWS sends it to the local
// callback on 127.0.0.1, which sends it back to /login/done (303). The
// token exchange and the profile registration finish in the background
// meanwhile, so /login/done waits for them.

// loginTimeout bounds a sign-in (the callback waits 10 minutes).
const loginTimeout = 11 * time.Minute

// loginAttempt is the sign-in in progress (one at a time).
type loginAttempt struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	result *LoginResult
	err    error
}

func (a *loginAttempt) finished() (bool, *LoginResult, error) {
	select {
	case <-a.done:
		a.mu.Lock()
		defer a.mu.Unlock()
		return true, a.result, a.err
	default:
		return false, nil, nil
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessWrite)
	if !ok {
		return
	}
	alias, err := text(r.PostForm, "alias", 128)
	if err != nil {
		s.flashAndRedirect(w, r, sess, fmt.Sprintf(tr(r, "invalidInput"), err.Error()), true, "/")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	a := &loginAttempt{cancel: cancel, done: make(chan struct{})}
	s.loginMu.Lock()
	if s.login != nil {
		s.login.cancel() // a new sign-in replaces one left unfinished
	}
	s.login = a
	s.loginMu.Unlock()

	authURL := make(chan string, 1)
	go func() {
		defer close(a.done)
		defer cancel()
		res, err := s.backend.Login(ctx, LoginRequest{Alias: alias, RedirectURL: s.origin + "/login/done"},
			func(u string) { authURL <- u })
		s.backend.InvalidateCredentials()
		a.mu.Lock()
		a.result, a.err = res, err
		a.mu.Unlock()
		if err != nil {
			s.logf("control panel: AWS sign-in failed: %v", err)
		} else {
			s.logf("control panel: signed in to AWS account %s", res.AccountID)
		}
	}()
	select {
	case u := <-authURL:
		if !strings.HasPrefix(u, "https://") {
			cancel()
			s.flashAndRedirect(w, r, sess, "unexpected sign-in address", true, "/")
			return
		}
		http.Redirect(w, r, u, http.StatusSeeOther)
	case <-a.done:
		_, _, err := a.finished()
		if err == nil {
			err = errors.New("the sign-in ended before it started")
		}
		s.flashAndRedirect(w, r, sess, err.Error(), true, "/")
	case <-time.After(30 * time.Second):
		cancel()
		s.flashAndRedirect(w, r, sess, "the sign-in did not start", true, "/")
	}
}

type loginDoneData struct {
	Waiting bool
	Err     string
}

// handleLoginDone is where the callback sends the browser back.
func (s *Server) handleLoginDone(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.check(w, r, accessPage)
	if !ok {
		return
	}
	s.loginMu.Lock()
	a := s.login
	s.loginMu.Unlock()
	if a == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	done, res, err := a.finished()
	switch {
	case !done:
		d := pageData{CSRF: sess.csrf, Nav: "home", Refresh: 1, Data: loginDoneData{Waiting: true}}
		s.render(w, r, http.StatusOK, "login_done", d)
	case err != nil:
		d := pageData{CSRF: sess.csrf, Nav: "home", Data: loginDoneData{Err: err.Error()}}
		setFormAction(w, "https://*.signin.aws.amazon.com")
		s.render(w, r, http.StatusOK, "login_done", d)
	default:
		s.flashAndRedirect(w, r, sess, fmt.Sprintf(tr(r, "signedInAs"), res.Alias, res.AccountID), false, "/")
	}
}
