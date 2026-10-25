package migration

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/problem"
	pg "github.com/pganalyze/pg_query_go/v6"
	query "github.com/wasilibs/go-pgquery"
	"github.com/wasilibs/go-pgquery/parser"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const MaxStatements = 50

// Statement menunjuk rentang byte input tervalidasi tanpa perubahan. SQL dijalankan dalam transaksi
// runner, bukan Exec mandiri.
type Statement struct {
	Index, Start, End int
	Kind              string
	SQL               string
}

// SQLValidationError hanya menampilkan kode dan diagnostic numerik. Position dihitung dari satu, atau
// nol jika tidak tersedia.
type SQLValidationError struct {
	Code           problem.Code
	StatementIndex int
	Position       int
}

func (e *SQLValidationError) Error() string { return string(e.Code) }
func (e *SQLValidationError) Unwrap() error { return &problem.Error{Code: e.Code} }

// ValidateSQL memeriksa sintaks PostgreSQL 17 dan policy migration M1 tanpa koneksi database atau
// pemeriksaan objek/izin. Saat error, tidak ada potongan SQL yang boleh dijalankan.
func ValidateSQL(sql string) ([]Statement, error) {
	invalid := func() ([]Statement, error) { return nil, &SQLValidationError{Code: problem.SQLInvalid} }
	if len(sql) == 0 || len(sql) > MaxSQLBytes || !utf8.ValidString(sql) || strings.ContainsRune(sql, 0) {
		return invalid()
	}
	tree, err := query.Parse(sql)
	if err != nil {
		failure := &SQLValidationError{Code: problem.SQLInvalid}
		var parseErr *parser.Error
		if errors.As(err, &parseErr) {
			failure.Position = parseErr.Cursorpos
		}
		return nil, failure
	}
	if len(tree.Stmts) == 0 {
		return invalid()
	}
	if tree.Version/10000 != 17 || len(tree.Stmts) > MaxStatements {
		return nil, &SQLValidationError{Code: problem.SQLUnsupported}
	}
	// PostgreSQL memotong identifier saat parsing. Periksa token scanner juga agar nama terlalu panjang
	// tidak diam-diam menunjuk objek lain.
	scan, err := query.Scan(sql)
	if err != nil {
		return invalid()
	}
	for _, token := range scan.Tokens {
		if token.Token == pg.Token_UIDENT {
			// Unicode dengan kutip biasa didukung. Sintaks escape U& berada di luar policy agar tidak diperlukan
			// decoder Unicode SQL kedua.
			return nil, &SQLValidationError{Code: problem.SQLUnsupported}
		}
		if token.Token != pg.Token_IDENT {
			continue
		}
		if token.Start < 0 || token.End <= token.Start || int(token.End) > len(sql) {
			return invalid()
		}
		name := sql[token.Start:token.End]
		if strings.HasPrefix(name, `"`) {
			name = strings.ReplaceAll(name[1:len(name)-1], `""`, `"`)
		}
		if !sqlIdentifier(name) {
			return nil, &SQLValidationError{Code: problem.SQLUnsupported}
		}
	}
	statements := make([]Statement, 0, len(tree.Stmts))
	previousEnd := 0
	for i, raw := range tree.Stmts {
		if raw == nil || raw.Stmt == nil {
			return invalid()
		}
		kind := statementKind(raw.Stmt)
		if kind == "" || !allowedSQL(raw.Stmt.ProtoReflect(), true) {
			return nil, &SQLValidationError{Code: problem.SQLUnsupported, StatementIndex: i + 1}
		}
		start, end := int(raw.StmtLocation), int(raw.StmtLocation)+int(raw.StmtLen)
		if raw.StmtLen == 0 {
			if i != len(tree.Stmts)-1 {
				return invalid()
			}
			end = len(sql)
		}
		if start < previousEnd || start < 0 || end <= start || end > len(sql) || !utf8.ValidString(sql[start:end]) {
			return invalid()
		}
		statements = append(statements, Statement{Index: i + 1, Start: start, End: end, Kind: kind, SQL: sql[start:end]})
		previousEnd = end
	}
	return statements, nil
}

func statementKind(n *pg.Node) string {
	switch n.Node.(type) {
	case *pg.Node_CreateStmt:
		return "CREATE TABLE"
	case *pg.Node_AlterTableStmt:
		return "ALTER TABLE"
	case *pg.Node_RenameStmt:
		return "RENAME"
	case *pg.Node_IndexStmt:
		return "CREATE INDEX"
	case *pg.Node_DropStmt:
		return "DROP"
	case *pg.Node_CreateSchemaStmt:
		return "CREATE SCHEMA"
	case *pg.Node_InsertStmt:
		return "INSERT"
	case *pg.Node_UpdateStmt:
		return "UPDATE"
	case *pg.Node_DeleteStmt:
		return "DELETE"
	default:
		return ""
	}
}

// Hanya field parser dalam daftar ini yang diterima. Node, field, atau protobuf unknown field lain
// ditolak, termasuk dalam ekspresi/CTE. Aturan terikat pada AST PostgreSQL 17 yang dipakai.
var sqlFields = map[protoreflect.Name]string{
	"CreateStmt":       "relation table_elts oncommit access_method if_not_exists",
	"AlterTableStmt":   "relation cmds objtype missing_ok",
	"AlterTableCmd":    "subtype name def behavior missing_ok recurse",
	"RenameStmt":       "rename_type relation_type relation subname newname behavior missing_ok",
	"IndexStmt":        "idxname relation access_method index_params unique nulls_not_distinct if_not_exists",
	"DropStmt":         "objects remove_type behavior missing_ok",
	"CreateSchemaStmt": "schemaname if_not_exists",
	"InsertStmt":       "relation cols select_stmt on_conflict_clause with_clause override",
	"UpdateStmt":       "relation target_list where_clause from_clause with_clause",
	"DeleteStmt":       "relation using_clause where_clause with_clause",
	"SelectStmt":       "distinct_clause target_list from_clause where_clause group_clause group_distinct having_clause window_clause values_lists sort_clause limit_offset limit_count limit_option with_clause op all larg rarg",
	"ColumnDef":        "colname type_name is_local is_not_null raw_default constraints location",
	"Constraint":       "contype conname deferrable initdeferred skip_validation initially_valid is_no_inherit raw_expr generated_when nulls_not_distinct keys pktable fk_attrs pk_attrs fk_matchtype fk_upd_action fk_del_action fk_del_set_cols location",
	"RangeVar":         "schemaname relname inh relpersistence alias location",
	"TypeName":         "names typmods typemod array_bounds location",
	"IndexElem":        "name expr collation opclass ordering nulls_ordering",
	"WithClause":       "ctes recursive location",
	"CommonTableExpr":  "ctename aliascolnames ctematerialized ctequery location",
	"OnConflictClause": "action infer target_list where_clause location",
	"InferClause":      "index_elems where_clause conname location",
	"Alias":            "aliasname colnames",
	"ResTarget":        "name indirection val location",
	"ColumnRef":        "fields location",
	"A_Expr":           "kind name lexpr rexpr location",
	"A_Const":          "ival fval boolval sval bsval isnull location",
	"Integer":          "ival", "Float": "fval", "Boolean": "boolval", "String": "sval", "BitString": "bsval",
	"List": "items", "A_Star": "",
	"BoolExpr":         "boolop args location",
	"FuncCall":         "funcname args agg_order agg_filter over agg_within_group agg_star agg_distinct func_variadic funcformat location",
	"NamedArgExpr":     "arg name argnumber location",
	"TypeCast":         "arg type_name location",
	"CollateClause":    "arg collname location",
	"A_ArrayExpr":      "elements location",
	"A_Indirection":    "arg indirection",
	"A_Indices":        "is_slice lidx uidx",
	"CaseExpr":         "arg args defresult location",
	"CaseWhen":         "expr result location",
	"CoalesceExpr":     "args location",
	"MinMaxExpr":       "op args location",
	"NullTest":         "arg nulltesttype argisrow location",
	"BooleanTest":      "arg booltesttype location",
	"SQLValueFunction": "op typmod location",
	"SubLink":          "sub_link_type testexpr oper_name subselect location",
	"RowExpr":          "args row_format location",
	"SetToDefault":     "location",
	"MultiAssignRef":   "source colno ncolumns",
	"JoinExpr":         "jointype is_natural larg rarg using_clause join_using_alias quals alias",
	"RangeSubselect":   "lateral subquery alias",
	"RangeFunction":    "lateral ordinality is_rowsfrom functions alias coldeflist",
	"SortBy":           "node sortby_dir sortby_nulls use_op location",
	"WindowDef":        "name refname partition_clause order_clause frame_options start_offset end_offset location",
	"GroupingSet":      "kind content location",
}

func allowedSQL(m protoreflect.Message, root bool) bool {
	if !m.IsValid() || len(m.GetUnknown()) != 0 {
		return false
	}
	if n, ok := m.Interface().(*pg.Node); ok {
		// Parser mewakili elemen NULL dengan Node kosong, termasuk SELECT DISTINCT dan daftar kolom opsional
		// range function.
		if n.Node == nil {
			return !root
		}
		field := m.WhichOneof(m.Descriptor().Oneofs().Get(0))
		return field != nil && allowedSQL(m.Get(field).Message(), root)
	}
	fields, ok := sqlFields[m.Descriptor().Name()]
	if !ok || !sqlShape(m.Interface(), root) {
		return false
	}
	valid := true
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if !strings.Contains(" "+fields+" ", " "+string(f.Name())+" ") {
			valid = false
			return false
		}
		if f.Kind() == protoreflect.MessageKind {
			if f.IsList() {
				list := v.List()
				for i := 0; i < list.Len(); i++ {
					if !allowedSQL(list.Get(i).Message(), false) {
						valid = false
						return false
					}
				}
			} else {
				valid = allowedSQL(v.Message(), false)
			}
		}
		return valid
	})
	return valid
}

func sqlIdentifier(s string) bool {
	return len(s) >= 1 && len(s) <= 63 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func userSchema(s string) bool {
	return sqlIdentifier(s) && !strings.HasPrefix(s, "pg_") && s != "information_schema"
}
func mutationRelation(r *pg.RangeVar) bool {
	return r != nil && r.Catalogname == "" && sqlIdentifier(r.Relname) && r.Relpersistence == "p" &&
		(r.Schemaname == "" && !strings.HasPrefix(r.Relname, "pg_") || userSchema(r.Schemaname))
}
func sqlNames(nodes []*pg.Node, max int) bool {
	if len(nodes) == 0 || len(nodes) > max {
		return false
	}
	for _, n := range nodes {
		if n.GetString_() == nil || !sqlIdentifier(n.GetString_().Sval) {
			return false
		}
	}
	return true
}

func sqlShape(message protoreflect.ProtoMessage, root bool) bool {
	switch n := message.(type) {
	case *pg.CreateStmt:
		if !root || !mutationRelation(n.Relation) || n.Oncommit != pg.OnCommitAction_ONCOMMIT_NOOP || n.AccessMethod != "" && n.AccessMethod != "heap" {
			return false
		}
		for _, elt := range n.TableElts {
			if c := elt.GetColumnDef(); c != nil {
				if !sqlIdentifier(c.Colname) || c.TypeName == nil {
					return false
				}
			} else if elt.GetConstraint() == nil {
				return false
			}
		}
	case *pg.AlterTableStmt:
		return root && mutationRelation(n.Relation) && n.Objtype == pg.ObjectType_OBJECT_TABLE && len(n.Cmds) > 0
	case *pg.AlterTableCmd:
		switch n.Subtype {
		case pg.AlterTableType_AT_AddColumn, pg.AlterTableType_AT_AlterColumnType:
			return n.Def.GetColumnDef() != nil
		case pg.AlterTableType_AT_AddConstraint:
			return n.Def.GetConstraint() != nil
		case pg.AlterTableType_AT_DropColumn, pg.AlterTableType_AT_ColumnDefault, pg.AlterTableType_AT_SetNotNull,
			pg.AlterTableType_AT_DropNotNull, pg.AlterTableType_AT_DropConstraint, pg.AlterTableType_AT_ValidateConstraint:
			return sqlIdentifier(n.Name)
		default:
			return false
		}
	case *pg.RenameStmt:
		if !root || !mutationRelation(n.Relation) || !sqlIdentifier(n.Newname) {
			return false
		}
		switch n.RenameType {
		case pg.ObjectType_OBJECT_TABLE:
			return true
		case pg.ObjectType_OBJECT_COLUMN:
			return n.RelationType == pg.ObjectType_OBJECT_TABLE && sqlIdentifier(n.Subname)
		case pg.ObjectType_OBJECT_TABCONSTRAINT:
			// PostgreSQL tidak mengisi relationType untuk bentuk sintaks ini.
			return sqlIdentifier(n.Subname)
		default:
			return false
		}
	case *pg.IndexStmt:
		if !root || !mutationRelation(n.Relation) || n.AccessMethod != "" && n.AccessMethod != "btree" || len(n.IndexParams) == 0 {
			return false
		}
		for _, item := range n.IndexParams {
			e := item.GetIndexElem()
			if e == nil || !sqlIdentifier(e.Name) || e.Expr != nil || len(e.Collation) != 0 || len(e.Opclass) != 0 {
				return false
			}
		}
	case *pg.DropStmt:
		if !root || len(n.Objects) == 0 {
			return false
		}
		for _, object := range n.Objects {
			if n.RemoveType == pg.ObjectType_OBJECT_SCHEMA {
				if object.GetString_() == nil || !userSchema(object.GetString_().Sval) {
					return false
				}
			} else if n.RemoveType == pg.ObjectType_OBJECT_TABLE || n.RemoveType == pg.ObjectType_OBJECT_INDEX {
				list := object.GetList().GetItems()
				if !sqlNames(list, 2) || len(list) == 2 && !userSchema(list[0].GetString_().Sval) || len(list) == 1 && strings.HasPrefix(list[0].GetString_().Sval, "pg_") {
					return false
				}
			} else {
				return false
			}
		}
	case *pg.CreateSchemaStmt:
		return root && userSchema(n.Schemaname)
	case *pg.InsertStmt:
		return root && mutationRelation(n.Relation) && n.Override == pg.OverridingKind_OVERRIDING_NOT_SET
	case *pg.UpdateStmt:
		return root && mutationRelation(n.Relation)
	case *pg.DeleteStmt:
		return root && mutationRelation(n.Relation)
	case *pg.RangeVar:
		return sqlIdentifier(n.Relname) && (n.Schemaname == "" || sqlIdentifier(n.Schemaname)) && n.Relpersistence == "p"
	case *pg.TypeName:
		return sqlNames(n.Names, 2)
	case *pg.FuncCall:
		return sqlNames(n.Funcname, 2)
	case *pg.ColumnRef:
		if len(n.Fields) == 0 || len(n.Fields) > 3 {
			return false
		}
		for i, field := range n.Fields {
			if field.GetAStar() != nil && i == len(n.Fields)-1 {
				continue
			}
			if field.GetString_() == nil || !sqlIdentifier(field.GetString_().Sval) {
				return false
			}
		}
	case *pg.Constraint:
		switch n.Contype {
		case pg.ConstrType_CONSTR_NULL, pg.ConstrType_CONSTR_NOTNULL, pg.ConstrType_CONSTR_DEFAULT,
			pg.ConstrType_CONSTR_IDENTITY, pg.ConstrType_CONSTR_GENERATED, pg.ConstrType_CONSTR_CHECK,
			pg.ConstrType_CONSTR_PRIMARY, pg.ConstrType_CONSTR_UNIQUE, pg.ConstrType_CONSTR_FOREIGN,
			pg.ConstrType_CONSTR_ATTR_DEFERRABLE, pg.ConstrType_CONSTR_ATTR_NOT_DEFERRABLE,
			pg.ConstrType_CONSTR_ATTR_DEFERRED, pg.ConstrType_CONSTR_ATTR_IMMEDIATE:
			return true
		default:
			return false
		}
	}
	return true
}
