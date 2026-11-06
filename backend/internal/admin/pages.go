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
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsActive    bool   `json:"is_active"`
	IsAdmin     bool   `json:"is_platform_admin"`
	Revision    int64  `json:"revision,string"`
}

// UserForm hanya berisi nilai yang aman ditampilkan ulang. Password tidak disalin ke data halaman.
type UserForm struct {
	Username         string
	DisplayName      string
	ExpectedRevision string
	IsActive         bool
	IsAdmin          bool
}

type GroupItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
	Revision int64  `json:"revision,string"`
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
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsActive    bool   `json:"is_active"`
}

type GrantItem struct {
	DatabaseID   string       `json:"database_id"`
	DatabaseName string       `json:"database_name"`
	Access       access.Level `json:"access"`
}

type DatabaseChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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
