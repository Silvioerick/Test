package auction

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Encryptor cifra as chaves de API guardadas no Postgres. A chave mestra
// (32 bytes, base64) é o único segredo que continua fora do banco — gere
// uma com `openssl rand -base64 32` e coloque em ENCRYPTION_KEY (ou
// ENCRYPTION_KEY_FILE apontando pro secret do Swarm). Guarde essa chave em
// lugar seguro: perdê-la significa perder acesso a toda chave de gateway
// já cadastrada no painel, e trocá-la invalida tudo que foi cifrado antes
// (tem que recadastrar as chaves de pagamento depois de trocar).
type Encryptor struct{ gcm cipher.AEAD }

func NewEncryptor(masterKeyBase64 string) (*Encryptor, error) {
	key, err := base64.StdEncoding.DecodeString(masterKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("ENCRYPTION_KEY inválida (esperado base64): %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("ENCRYPTION_KEY precisa decodificar pra 32 bytes (AES-256), tem %d — gere com: openssl rand -base64 32", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Encryptor{gcm: gcm}, nil
}

func (e *Encryptor) Encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, e.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return e.gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func (e *Encryptor) Decrypt(data []byte) (string, error) {
	ns := e.gcm.NonceSize()
	if len(data) < ns {
		return "", errors.New("auction: dado cifrado corrompido ou curto demais")
	}
	nonce, ciphertext := data[:ns], data[ns:]
	plain, err := e.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("auction: falha ao decifrar (chave mestra errada ou dado corrompido): %w", err)
	}
	return string(plain), nil
}
