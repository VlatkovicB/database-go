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

	// Symbols
	ASTERISK  // *
	COMMA     // ,
	SEMICOLON // ;
	LPAREN    // (
	RPAREN    // )
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
}

type Token struct {
	Type    TokenType
	Literal string
}
