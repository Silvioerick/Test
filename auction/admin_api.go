package auction

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const adminCookie = "auction_admin"

// --- autenticação do painel -------------------------------------------
//
// Duas formas de provar quem é, nesta ordem:
//
//  1. Cookie de sessão, obtido em POST /api/admin/login com usuário e
//     senha. É o caminho das pessoas. httpOnly, então JavaScript nenhum
//     (inclusive um XSS) consegue ler o valor.
//  2. Authorization: Bearer <ADMIN_TOKEN>, para script e automação.
//     Opcional: sem ADMIN_TOKEN definido, só o login funciona.

// adminFromRequest devolve o operador autenticado, ou erro.
func (s *APIServer) adminFromRequest(r *http.Request) (AdminUser, error) {
	if c, err := r.Cookie(adminCookie); err == nil && c.Value != "" {
		return s.store.AdminBySession(r.Context(), c.Value)
	}
	return AdminUser{}, ErrAdminSessionBad
}

func (s *APIServer) setAdminCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		// Secure quando servido por HTTPS (o proxy encerra o TLS e manda
		// X-Forwarded-Proto). Em HTTP puro o cookie precisa funcionar,
		// senão não dá para usar o painel numa rede interna.
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *APIServer) clearAdminCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
}

type adminDTO struct {
	ID          int64      `json:"id"`
	Username    string     `json:"username"`
	MustChange  bool       `json:"must_change"`
	Disabled    bool       `json:"disabled,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func toAdminDTO(u AdminUser) adminDTO {
	d := adminDTO{ID: u.ID, Username: u.Username, MustChange: u.MustChange,
		Disabled: u.Disabled, CreatedAt: u.CreatedAt}
	if u.LastLoginAt.Valid {
		d.LastLoginAt = &u.LastLoginAt.Time
	}
	return d
}

func (s *APIServer) adminLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	// Trava por origem, além da trava por usuário que o store aplica.
	if !s.loginIPs.allow("admin:" + clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "muitas tentativas, aguarde alguns minutos")
		return
	}
	u, err := s.store.AuthenticateAdmin(r.Context(), in.Username, in.Password)
	switch {
	case errors.Is(err, ErrAdminInvalid), errors.Is(err, ErrAdminDisabled):
		// Mensagem única: não dizemos se o usuário existe.
		writeErr(w, http.StatusUnauthorized, "usuário ou senha inválidos")
		return
	case errors.Is(err, ErrAdminLocked):
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	case err != nil:
		s.fail(w, "login do painel", err)
		return
	}
	token, expires, err := s.store.CreateAdminSession(r.Context(), u.ID)
	if err != nil {
		s.fail(w, "abrir sessão do painel", err)
		return
	}
	s.setAdminCookie(w, r, token, expires)
	s.logf("auction api: login do painel: %s (de %s)", u.Username, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"username": u.Username, "must_change": u.MustChange,
	})
}

func (s *APIServer) adminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminCookie); err == nil && c.Value != "" {
		if err := s.store.RevokeAdminSession(r.Context(), c.Value); err != nil {
			s.logf("auction api: revogar sessão do painel: %v", err)
		}
	}
	s.clearAdminCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *APIServer) adminMe(w http.ResponseWriter, r *http.Request) {
	u, err := s.adminFromRequest(r)
	if err != nil {
		// Token de automação não tem usuário associado.
		if s.bearerOK(r) {
			writeJSON(w, http.StatusOK, map[string]any{"username": "(token de automação)", "token_auth": true})
			return
		}
		writeErr(w, http.StatusUnauthorized, "não autenticado")
		return
	}
	writeJSON(w, http.StatusOK, toAdminDTO(u))
}

func (s *APIServer) adminChangePassword(w http.ResponseWriter, r *http.Request) {
	u, err := s.adminFromRequest(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "faça login para trocar a senha")
		return
	}
	var in struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	c, _ := r.Cookie(adminCookie)
	keep := ""
	if c != nil {
		keep = c.Value
	}
	err = s.store.ChangeAdminPassword(r.Context(), u.ID, in.Current, in.Next, keep)
	switch {
	case errors.Is(err, ErrAdminInvalid):
		writeErr(w, http.StatusUnauthorized, "senha atual incorreta")
	case errors.Is(err, ErrWeakPassword):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.fail(w, "trocar senha", err)
	default:
		s.logf("auction api: senha trocada: %s", u.Username)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (s *APIServer) adminListUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListAdmins(r.Context())
	if err != nil {
		s.fail(w, "listar operadores", err)
		return
	}
	out := make([]adminDTO, 0, len(list))
	for _, u := range list {
		out = append(out, toAdminDTO(u))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *APIServer) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	id, err := s.store.CreateAdmin(r.Context(), in.Username, in.Password, true)
	switch {
	case errors.Is(err, ErrWeakPassword):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil && strings.Contains(err.Error(), "duplicate key"):
		writeErr(w, http.StatusConflict, "já existe um operador com esse usuário")
	case err != nil:
		s.fail(w, "criar operador", err)
	default:
		s.logf("auction api: operador criado: %s", strings.ToLower(in.Username))
		writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	}
}

func (s *APIServer) adminSetUserDisabled(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id inválido")
		return
	}
	var in struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "json inválido")
		return
	}
	// Não deixa desativar a si mesmo nem o último operador ativo, senão
	// ninguém mais entra no painel.
	if me, err := s.adminFromRequest(r); err == nil && me.ID == id && in.Disabled {
		writeErr(w, http.StatusBadRequest, "você não pode desativar a si mesmo")
		return
	}
	if in.Disabled {
		n, err := s.store.CountAdmins(r.Context())
		if err != nil {
			s.fail(w, "contar operadores", err)
			return
		}
		if n <= 1 {
			writeErr(w, http.StatusBadRequest, "é o último operador ativo — crie outro antes de desativar este")
			return
		}
	}
	if err := s.store.SetAdminDisabled(r.Context(), id, in.Disabled); err != nil {
		s.fail(w, "ativar/desativar operador", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
