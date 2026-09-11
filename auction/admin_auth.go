package auction

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var (
	ErrAdminInvalid    = errors.New("auction: usuário ou senha inválidos")
	ErrAdminLocked     = errors.New("auction: muitas tentativas, tente de novo em alguns minutos")
	ErrAdminDisabled   = errors.New("auction: usuário desativado")
	ErrWeakPassword    = errors.New("auction: senha fraca")
	ErrAdminSessionBad = errors.New("auction: sessão do painel inválida ou expirada")
)

const (
	// OWASP recomenda 600k iterações para PBKDF2-HMAC-SHA256 (2023).
	pbkdf2Iter    = 600_000
	pbkdf2KeyLen  = 32
	pbkdf2SaltLen = 16

	adminSessionTTL     = 12 * time.Hour
	adminLoginWindow    = 15 * time.Minute
	adminMaxFailedTries = 8
	minPasswordLen      = 10
)

// HashPassword deriva a senha com PBKDF2-HMAC-SHA256 e devolve no formato
// pbkdf2_sha256$<iterações>$<salt>$<hash>, que guarda os parâmetros junto
// — assim dá para aumentar o custo no futuro sem invalidar o que existe.
func HashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iter, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s", pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword confere em tempo constante.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// CheckPasswordStrength recusa o óbvio. Não tenta ser um medidor
// sofisticado: exige comprimento e alguma variedade, que é o que evita as
// senhas que caem em segundos.
func CheckPasswordStrength(p string) error {
	if len([]rune(p)) < minPasswordLen {
		return fmt.Errorf("%w: use pelo menos %d caracteres", ErrWeakPassword, minPasswordLen)
	}
	var hasLetter, hasOther bool
	for _, r := range p {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r), unicode.IsPunct(r), unicode.IsSymbol(r), unicode.IsSpace(r):
			hasOther = true
		}
	}
	if !hasLetter || !hasOther {
		return fmt.Errorf("%w: misture letras com números ou símbolos", ErrWeakPassword)
	}
	for _, comum := range []string{"12345678", "password", "senha123", "admin123", "qwerty"} {
		if strings.Contains(strings.ToLower(p), comum) {
			return fmt.Errorf("%w: essa sequência é das primeiras que um atacante tenta", ErrWeakPassword)
		}
	}
	return nil
}

type AdminUser struct {
	ID          int64
	Username    string
	MustChange  bool
	LastLoginAt sql.NullTime
	CreatedAt   time.Time
	Disabled    bool
}

// CreateAdmin cadastra um operador do painel.
func (s *Store) CreateAdmin(ctx context.Context, username, password string, mustChange bool) (int64, error) {
	username = strings.TrimSpace(strings.ToLower(username))
	if username == "" {
		return 0, errors.New("auction: usuário é obrigatório")
	}
	if err := CheckPasswordStrength(password); err != nil {
		return 0, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO admin_users (username, password_hash, must_change)
		VALUES ($1, $2, $3) RETURNING id`, username, hash, mustChange).Scan(&id)
	return id, err
}

// CountAdmins diz se já existe algum operador — usado pelo bootstrap.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM admin_users WHERE disabled_at IS NULL`).Scan(&n)
	return n, err
}

// AuthenticateAdmin confere usuário e senha, com trava de força bruta por
// usuário. Sempre gasta o mesmo tempo em usuário inexistente e em senha
// errada, para o tempo de resposta não revelar quais usuários existem.
func (s *Store) AuthenticateAdmin(ctx context.Context, username, password string) (AdminUser, error) {
	username = strings.TrimSpace(strings.ToLower(username))

	var failed int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM admin_login_attempts
		WHERE username = $1 AND NOT ok AND at > now() - ($2 || ' milliseconds')::interval`,
		username, adminLoginWindow.Milliseconds()).Scan(&failed); err != nil {
		return AdminUser{}, err
	}
	if failed >= adminMaxFailedTries {
		return AdminUser{}, ErrAdminLocked
	}

	var u AdminUser
	var hash string
	var disabledAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, password_hash, must_change, disabled_at, last_login_at, created_at
		FROM admin_users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &hash, &u.MustChange, &disabledAt, &u.LastLoginAt, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// Gasta o mesmo trabalho de uma verificação real: sem isto, o
		// tempo de resposta diria quais usuários existem.
		VerifyPassword("pbkdf2_sha256$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password)
		s.recordAdminAttempt(ctx, username, false)
		return AdminUser{}, ErrAdminInvalid
	}
	if err != nil {
		return AdminUser{}, err
	}
	if !VerifyPassword(hash, password) {
		s.recordAdminAttempt(ctx, username, false)
		return AdminUser{}, ErrAdminInvalid
	}
	if disabledAt.Valid {
		s.recordAdminAttempt(ctx, username, false)
		return AdminUser{}, ErrAdminDisabled
	}
	s.recordAdminAttempt(ctx, username, true)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE admin_users SET last_login_at = now() WHERE id = $1`, u.ID); err != nil {
		return AdminUser{}, err
	}
	return u, nil
}

func (s *Store) recordAdminAttempt(ctx context.Context, username string, ok bool) {
	//nolint:errcheck // registrar a tentativa não pode derrubar o login
	s.db.ExecContext(ctx,
		`INSERT INTO admin_login_attempts (username, ok) VALUES ($1, $2)`, username, ok)
}

// CreateAdminSession abre a sessão do painel. Guarda só o hash do token.
func (s *Store) CreateAdminSession(ctx context.Context, adminID int64) (token string, expires time.Time, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	token = hex.EncodeToString(buf)
	expires = time.Now().Add(adminSessionTTL)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO admin_sessions (token_hash, admin_id, expires_at)
		VALUES ($1, $2, now() + ($3 || ' milliseconds')::interval)`,
		hashCode(token), adminID, adminSessionTTL.Milliseconds())
	return token, expires, err
}

// AdminBySession valida o cookie de sessão.
func (s *Store) AdminBySession(ctx context.Context, token string) (AdminUser, error) {
	var u AdminUser
	var disabledAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT a.id, a.username, a.must_change, a.disabled_at, a.last_login_at, a.created_at
		FROM admin_sessions s JOIN admin_users a ON a.id = s.admin_id
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now()`,
		hashCode(token)).Scan(&u.ID, &u.Username, &u.MustChange, &disabledAt, &u.LastLoginAt, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminUser{}, ErrAdminSessionBad
	}
	if err != nil {
		return AdminUser{}, err
	}
	if disabledAt.Valid {
		return AdminUser{}, ErrAdminDisabled
	}
	return u, nil
}

// RevokeAdminSession encerra uma sessão (logout).
func (s *Store) RevokeAdminSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE admin_sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`,
		hashCode(token))
	return err
}

// ChangeAdminPassword troca a senha e derruba as OUTRAS sessões da pessoa
// — se a troca foi por suspeita de vazamento, deixar as antigas valendo
// anularia o efeito.
func (s *Store) ChangeAdminPassword(ctx context.Context, adminID int64, current, next, keepToken string) error {
	var hash string
	if err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM admin_users WHERE id = $1`, adminID).Scan(&hash); err != nil {
		return err
	}
	if !VerifyPassword(hash, current) {
		return ErrAdminInvalid
	}
	if err := CheckPasswordStrength(next); err != nil {
		return err
	}
	if VerifyPassword(hash, next) {
		return fmt.Errorf("%w: a nova senha é igual à atual", ErrWeakPassword)
	}
	newHash, err := HashPassword(next)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE admin_users SET password_hash = $2, must_change = false WHERE id = $1`,
		adminID, newHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE admin_sessions SET revoked_at = now()
		WHERE admin_id = $1 AND revoked_at IS NULL AND token_hash <> $2`,
		adminID, hashCode(keepToken)); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetAdminPassword redefine a senha de um operador sem pedir a atual.
// É a saída para o esquecimento: roda pela linha de comando no servidor,
// onde quem executa já tem acesso à máquina e ao banco. Derruba todas as
// sessões daquele operador.
func (s *Store) ResetAdminPassword(ctx context.Context, username, newPassword string) error {
	username = strings.TrimSpace(strings.ToLower(username))
	if err := CheckPasswordStrength(newPassword); err != nil {
		return err
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `
		UPDATE admin_users SET password_hash = $2, must_change = true, disabled_at = NULL
		WHERE username = $1 RETURNING id`, username, hash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("auction: operador %q não existe", username)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE admin_sessions SET revoked_at = now() WHERE admin_id = $1 AND revoked_at IS NULL`,
		id); err != nil {
		return err
	}
	// Limpa a trava de força bruta: quem redefiniu precisa conseguir entrar.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM admin_login_attempts WHERE username = $1`, username); err != nil {
		return err
	}
	return tx.Commit()
}

// ListAdmins lista os operadores do painel.
func (s *Store) ListAdmins(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, must_change, disabled_at IS NOT NULL, last_login_at, created_at
		FROM admin_users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminUser
	for rows.Next() {
		var u AdminUser
		if err := rows.Scan(&u.ID, &u.Username, &u.MustChange, &u.Disabled, &u.LastLoginAt, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetAdminDisabled ativa ou desativa um operador e derruba as sessões
// dele quando desativa.
func (s *Store) SetAdminDisabled(ctx context.Context, adminID int64, disabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if disabled {
		if _, err := tx.ExecContext(ctx,
			`UPDATE admin_users SET disabled_at = now() WHERE id = $1`, adminID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE admin_sessions SET revoked_at = now() WHERE admin_id = $1 AND revoked_at IS NULL`,
			adminID); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx,
		`UPDATE admin_users SET disabled_at = NULL WHERE id = $1`, adminID); err != nil {
		return err
	}
	return tx.Commit()
}

// PurgeAdminAuth limpa sessões e tentativas antigas.
func (s *Store) PurgeAdminAuth(ctx context.Context, olderThan time.Duration) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM admin_sessions WHERE expires_at < now() - ($1 || ' milliseconds')::interval`,
		olderThan.Milliseconds()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM admin_login_attempts WHERE at < now() - ($1 || ' milliseconds')::interval`,
		olderThan.Milliseconds())
	return err
}
