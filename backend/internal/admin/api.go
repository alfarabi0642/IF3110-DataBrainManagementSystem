package admin

import (
	"math"
	"net/http"
	"net/url"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/auth"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
)

// RegisterAPI memakai service yang sama tanpa dependency layout atau renderer.
func (s *Service) RegisterAPI(mux *http.ServeMux, middleware *auth.Middleware) {
	read := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, middleware.RequireSession(s.adminAPI(handler)))
	}
	post := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, middleware.RequireSession(httpx.LimitBody(httpx.NormalBodyLimit, middleware.RequireCSRF(s.adminAPI(handler)))))
	}
	read("GET /api/admin/users", s.usersAPI)
	read("GET /api/admin/users/{user_id}", s.userAPI)
	read("GET /api/admin/groups", s.groupsAPI)
	read("GET /api/admin/groups/{group_id}", s.groupAPI)
	post("POST /api/admin/users", s.createUserAPI)
	post("POST /api/admin/users/{user_id}", s.editUserAPI)
	post("POST /api/admin/users/{user_id}/password", s.passwordAPI)
	post("POST /api/admin/groups", s.createGroupAPI)
	post("POST /api/admin/groups/{group_id}", s.editGroupAPI)
	post("POST /api/admin/groups/{group_id}/members", s.memberAPI)
	post("POST /api/admin/groups/{group_id}/members/{user_id}/remove", s.memberAPI)
	post("POST /api/admin/groups/{group_id}/databases", s.grantAPI)
	post("POST /api/admin/groups/{group_id}/databases/{database_id}/remove", s.grantAPI)
}

func (s *Service) adminAPI(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := access.FromContext(r.Context())
		if !ok {
			httpx.WriteJSONError(w, r, failure(problem.SessionInvalid))
			return
		}
		if err := s.authorizer.RequireAdmin(r.Context(), actor); err != nil {
			httpx.WriteJSONError(w, r, err)
			return
		}
		next(w, r)
	})
}

func (s *Service) usersAPI(w http.ResponseWriter, r *http.Request) {
	actor, _ := access.FromContext(r.Context())
	limit, offset, err := parsePagination(r)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	items, more, err := s.ListUsers(r.Context(), actor, limit, offset)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, items, httpx.NewPage(limit, offset, more))
}

func (s *Service) groupsAPI(w http.ResponseWriter, r *http.Request) {
	actor, _ := access.FromContext(r.Context())
	limit, offset, err := parsePagination(r)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	items, more, err := s.ListGroups(r.Context(), actor, limit, offset)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, items, httpx.NewPage(limit, offset, more))
}

func (s *Service) userAPI(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "user_id")
	if err == nil {
		_, err = httpx.ParseQuery(r)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	actor, _ := access.FromContext(r.Context())
	user, groups, err := s.User(r.Context(), actor, id)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, struct {
		UserItem
		Groups []GroupItem `json:"groups"`
	}{user, groups}, nil)
}

func (s *Service) groupAPI(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "group_id")
	if err == nil {
		_, err = httpx.ParseQuery(r)
	}
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	actor, _ := access.FromContext(r.Context())
	group, members, choices, grants, databases, err := s.GroupDetails(r.Context(), actor, id)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, struct {
		GroupItem
		Members         []MemberItem     `json:"members"`
		MemberChoices   []MemberItem     `json:"member_choices"`
		Grants          []GrantItem      `json:"grants"`
		DatabaseChoices []DatabaseChoice `json:"database_choices"`
	}{group, members, choices, grants, databases}, nil)
}

func (s *Service) createUserAPI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
		Admin       bool   `json:"is_platform_admin"`
	}
	err := httpx.DecodeJSON(w, r, httpx.NormalBodyLimit, &in, []string{"username", "display_name", "password", "is_platform_admin"}, nil)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	actor, _ := access.FromContext(r.Context())
	id, err := s.CreateUser(r.Context(), actor, CreateUserInput{in.Username, in.DisplayName, in.Password, in.Admin})
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/admin/users/"+id.String())
	httpx.WriteJSON(w, r, 201, map[string]string{"id": id.String()}, nil)
}

func (s *Service) editUserAPI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision    string `json:"expected_revision"`
		DisplayName string `json:"display_name"`
		Active      bool   `json:"is_active"`
		Admin       bool   `json:"is_platform_admin"`
	}
	err := httpx.DecodeJSON(w, r, httpx.NormalBodyLimit, &in, []string{"expected_revision", "display_name", "is_active", "is_platform_admin"}, nil)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	id, err := pathID(r, "user_id")
	var revision int64
	if err == nil {
		revision, err = apiRevision(in.Revision)
	}
	actor, _ := access.FromContext(r.Context())
	if err == nil {
		_, err = s.EditUser(r.Context(), actor, id, EditUserInput{revision, in.DisplayName, in.Active, in.Admin})
	}
	apiMutation(w, r, err)
}

func (s *Service) passwordAPI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision string `json:"expected_revision"`
		Password string `json:"password"`
	}
	err := httpx.DecodeJSON(w, r, httpx.NormalBodyLimit, &in, []string{"expected_revision", "password"}, nil)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	id, err := pathID(r, "user_id")
	var revision int64
	if err == nil {
		revision, err = apiRevision(in.Revision)
	}
	actor, _ := access.FromContext(r.Context())
	if err == nil {
		_, err = s.ResetPassword(r.Context(), actor, id, ResetPasswordInput{revision, in.Password})
	}
	apiMutation(w, r, err)
}

func (s *Service) createGroupAPI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	err := httpx.DecodeJSON(w, r, httpx.NormalBodyLimit, &in, []string{"name"}, nil)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	actor, _ := access.FromContext(r.Context())
	id, err := s.CreateGroup(r.Context(), actor, CreateGroupInput{in.Name})
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/admin/groups/"+id.String())
	httpx.WriteJSON(w, r, 201, map[string]string{"id": id.String()}, nil)
}

func (s *Service) editGroupAPI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision string `json:"expected_revision"`
		Name     string `json:"name"`
		Active   bool   `json:"is_active"`
	}
	err := httpx.DecodeJSON(w, r, httpx.NormalBodyLimit, &in, []string{"expected_revision", "name", "is_active"}, nil)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	id, err := pathID(r, "group_id")
	var revision int64
	if err == nil {
		revision, err = apiRevision(in.Revision)
	}
	actor, _ := access.FromContext(r.Context())
	if err == nil {
		_, err = s.EditGroup(r.Context(), actor, id, EditGroupInput{revision, in.Name, in.Active})
	}
	apiMutation(w, r, err)
}

func (s *Service) memberAPI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UserID string `json:"user_id"`
	}
	remove := r.PathValue("user_id") != ""
	var required []string
	if !remove {
		required = []string{"user_id"}
	}
	err := httpx.DecodeJSON(w, r, httpx.NormalBodyLimit, &in, required, nil)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	if remove {
		in.UserID = r.PathValue("user_id")
	}
	id, err := pathID(r, "group_id")
	userID, userErr := parsePathID(in.UserID)
	if err == nil {
		err = userErr
	}
	actor, _ := access.FromContext(r.Context())
	if err == nil {
		if remove {
			_, err = s.RemoveMember(r.Context(), actor, id, userID)
		} else {
			_, err = s.AddMember(r.Context(), actor, id, userID)
		}
	}
	apiMutation(w, r, err)
}

func (s *Service) grantAPI(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DatabaseID string       `json:"database_id"`
		Access     access.Level `json:"access_level"`
	}
	remove := r.PathValue("database_id") != ""
	var required []string
	if !remove {
		required = []string{"database_id", "access_level"}
	}
	err := httpx.DecodeJSON(w, r, httpx.NormalBodyLimit, &in, required, nil)
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	if remove {
		in.DatabaseID = r.PathValue("database_id")
	}
	id, err := pathID(r, "group_id")
	target, targetErr := parsePathID(in.DatabaseID)
	if err == nil {
		err = targetErr
	}
	actor, _ := access.FromContext(r.Context())
	if err == nil {
		if remove {
			_, err = s.RemoveGrant(r.Context(), actor, id, target)
		} else {
			_, err = s.SetGrant(r.Context(), actor, id, target, in.Access)
		}
	}
	apiMutation(w, r, err)
}

func apiRevision(value string) (int64, error) {
	return httpx.Int64Value(url.Values{"expected_revision": {value}}, "expected_revision", 1, math.MaxInt64)
}

func apiMutation(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		httpx.WriteJSONError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, 200, struct{}{}, nil)
}
