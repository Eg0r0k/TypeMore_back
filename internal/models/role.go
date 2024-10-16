package models

type Role int8

const (
	InvalidRole Role = iota - 1
	UserRole
	AdminRole
	SuperAdminRole
)

func (r Role) Valid() bool {
	switch r {
	case UserRole:
		break
	case AdminRole:
		break
	case SuperAdminRole:
		break
	default:
		return false
	}
	return true
}
func (r Role) String() string {
	switch r {
	case UserRole:
		return "user"
	case AdminRole:
		return "admin"
	case SuperAdminRole:
		return "super_admin"
	default:
		return "invalid"
	}
}
func RoleFromString(roleStr string) Role {
	switch roleStr {
	case "user":
		return UserRole
	case "admin":
		return AdminRole
	case "super_admin":
		return SuperAdminRole
	default:
		return InvalidRole
	}
}
