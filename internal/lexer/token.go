package lexer

type TokenType int

const (
	ILLEGAL TokenType = iota
	EOF

	// Literals
	IDENT
	INT_LIT
	FLOAT_LIT
	STRING_LIT

	// Keywords
	SELECT
	FROM
	WHERE
	INSERT
	INTO
	VALUES
	UPDATE
	SET
	DELETE
	CREATE
	TABLE
	DROP
	AND
	OR
	NOT
	NULL
	TRUE
	FALSE
	INT
	TEXT
	BOOLEAN
	FLOAT
	PRIMARY
	KEY

	// Join keywords
	JOIN
	INNER
	LEFT
	OUTER
	ON
	AS

	// Grouping keywords
	GROUP
	BY
	HAVING

	// Symbols
	ASTERISK  // *
	COMMA     // ,
	SEMICOLON // ;
	LPAREN    // (
	RPAREN    // )
	DOT       // .
	EQ        // =
	NEQ       // !=
	LT        // <
	GT        // >
	LTE       // <=
	GTE       // >=
)

var keywords = map[string]TokenType{
	"SELECT":  SELECT,
	"FROM":    FROM,
	"WHERE":   WHERE,
	"INSERT":  INSERT,
	"INTO":    INTO,
	"VALUES":  VALUES,
	"UPDATE":  UPDATE,
	"SET":     SET,
	"DELETE":  DELETE,
	"CREATE":  CREATE,
	"TABLE":   TABLE,
	"DROP":    DROP,
	"AND":     AND,
	"OR":      OR,
	"NOT":     NOT,
	"NULL":    NULL,
	"TRUE":    TRUE,
	"FALSE":   FALSE,
	"INT":     INT,
	"TEXT":    TEXT,
	"BOOLEAN": BOOLEAN,
	"FLOAT":   FLOAT,
	"PRIMARY": PRIMARY,
	"KEY":     KEY,
	"JOIN":    JOIN,
	"INNER":   INNER,
	"LEFT":    LEFT,
	"OUTER":   OUTER,
	"ON":      ON,
	"AS":      AS,
	"GROUP":   GROUP,
	"BY":      BY,
	"HAVING":  HAVING,
}

type Token struct {
	Type    TokenType
	Literal string
}

var tokenNames = map[TokenType]string{
	ILLEGAL: "ILLEGAL", EOF: "EOF", IDENT: "IDENT",
	INT_LIT: "INT_LIT", FLOAT_LIT: "FLOAT_LIT", STRING_LIT: "STRING_LIT",
	SELECT: "SELECT", FROM: "FROM", WHERE: "WHERE", INSERT: "INSERT",
	INTO: "INTO", VALUES: "VALUES", UPDATE: "UPDATE", SET: "SET",
	DELETE: "DELETE", CREATE: "CREATE", TABLE: "TABLE", DROP: "DROP",
	AND: "AND", OR: "OR", NOT: "NOT", NULL: "NULL", TRUE: "TRUE", FALSE: "FALSE",
	INT: "INT", TEXT: "TEXT", BOOLEAN: "BOOLEAN", FLOAT: "FLOAT",
	PRIMARY: "PRIMARY", KEY: "KEY",
	JOIN: "JOIN", INNER: "INNER", LEFT: "LEFT", OUTER: "OUTER", ON: "ON", AS: "AS",
	GROUP: "GROUP", BY: "BY", HAVING: "HAVING",
	ASTERISK: "*", COMMA: ",", SEMICOLON: ";", LPAREN: "(", RPAREN: ")",
	DOT: ".", EQ: "=", NEQ: "!=", LT: "<", GT: ">", LTE: "<=", GTE: ">=",
}

func (t TokenType) Name() string {
	if n, ok := tokenNames[t]; ok {
		return n
	}
	return "UNKNOWN"
}

func (t TokenType) Category() string {
	switch t {
	case SELECT, FROM, WHERE, INSERT, INTO, VALUES, UPDATE, SET, DELETE, CREATE, TABLE, DROP, AND, OR, NOT, NULL, TRUE, FALSE,
		JOIN, INNER, LEFT, OUTER, ON, AS, GROUP, BY, HAVING:
		return "keyword"
	case INT, TEXT, BOOLEAN, FLOAT, PRIMARY, KEY:
		return "type"
	case IDENT:
		return "ident"
	case INT_LIT, FLOAT_LIT, STRING_LIT:
		return "literal"
	case EQ, NEQ, LT, GT, LTE, GTE:
		return "operator"
	case ASTERISK, COMMA, SEMICOLON, LPAREN, RPAREN, DOT:
		return "punct"
	default:
		return "other"
	}
}
