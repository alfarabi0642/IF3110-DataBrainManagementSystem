package admin

import (
	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/access"
)

type UserItem struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsActive    bool   `json:"is_active"`
	IsAdmin     bool   `json:"is_platform_admin"`
	Revision    int64  `json:"revision,string"`
}

type GroupItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
	Revision int64  `json:"revision,string"`
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
