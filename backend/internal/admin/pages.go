package admin

import (
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
)

type Pager struct {
	Prev string
	Next string
}

type UserItem struct {
	ID          string
	Username    string
	DisplayName string
	IsActive    bool
	IsAdmin     bool
	Revision    int64
}

// UserForm contains only values safe to redisplay. Passwords stay in request
// handling and are never copied into a page value.
type UserForm struct {
	Username         string
	DisplayName      string
	ExpectedRevision string
	IsActive         bool
	IsAdmin          bool
}

type GroupItem struct {
	ID       string
	Name     string
	IsActive bool
	Revision int64
}

type UsersPage struct {
	httpx.Layout
	Users []UserItem
	Pager Pager
	Form  UserForm
	Error string
}

type UserPage struct {
	httpx.Layout
	UserProfile UserItem
	Groups      []GroupItem
	Form        UserForm
	Error       string
	Conflict    bool
}

type GroupForm struct {
	Name             string
	ExpectedRevision string
	IsActive         bool
}

type MemberItem struct {
	ID          string
	Username    string
	DisplayName string
	IsActive    bool
}

type GrantItem struct {
	DatabaseID   string
	DatabaseName string
	Access       access.Level
}

type DatabaseChoice struct {
	ID   string
	Name string
}

type GroupsPage struct {
	httpx.Layout
	Groups []GroupItem
	Pager  Pager
	Form   GroupForm
	Error  string
}

type GroupPage struct {
	httpx.Layout
	GroupProfile    GroupItem
	Form            GroupForm
	Members         []MemberItem
	MemberChoices   []MemberItem
	Grants          []GrantItem
	DatabaseChoices []DatabaseChoice
	Error           string
	Conflict        bool
}
