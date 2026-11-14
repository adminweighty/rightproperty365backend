package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// The registration captcha: a plain arithmetic question that a person answers
// before an account is created. It is built in (no third-party service, no
// secret): the question is generated here, only a hash of the answer is kept
// server side, and a captcha can be presented once.
//
// It is a speed bump for scripts, not a strong bot defence: anything that
// reads the question can answer it. The per-IP rate limits stay in place.

// CaptchaTTL is how long a registration captcha can be answered.
const CaptchaTTL = 10 * time.Minute

const (
	// The largest operand of a question. Sums stay at or below 20.
	captchaMaxOperand = 10
	// No answer has more digits than this; longer input is simply wrong.
	captchaMaxAnswerDigits = 4
)

// Messages for the "captcha" field. Plain language: they are shown to people.
const (
	msgCaptchaRequired = "Answer the question to show you are a person."
	msgCaptchaWrong    = "That answer is not right. Try the new question."
	msgCaptchaExpired  = "That question has expired. Try the new question."
)

// CaptchaInput is the "captcha" object of POST /v1/auth/register.
type CaptchaInput struct {
	CaptchaID string `json:"captcha_id"`
	Answer    string `json:"answer"`
}

// CaptchaChallenge is the body of GET /v1/auth/captcha.
type CaptchaChallenge struct {
	CaptchaID string    `json:"captcha_id"`
	Question  string    `json:"question"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Captcha is a stored registration captcha. The answer itself is never
// stored, only its hash.
type Captcha struct {
	ID         string
	AnswerHash []byte
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

// captchaAnswerHash binds the answer to its captcha, so equal answers of
// different captchas do not share a hash. The answer space is tiny, so this
// hides nothing from someone who can read the table; it only keeps the answer
// out of plain sight. What protects the captcha is that it can be used once.
func captchaAnswerHash(id string, answer int) []byte {
	sum := sha256.Sum256([]byte("registration-captcha:" + id + ":" + strconv.Itoa(answer)))
	return sum[:]
}

// randomInt returns a uniform number in [0, n).
func randomInt(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, fmt.Errorf("generate captcha number: %w", err)
	}
	return int(v.Int64()), nil
}

// newCaptchaQuestion returns a question and its answer: an addition, or a
// subtraction whose result is never negative, of numbers from 1 to 10.
func newCaptchaQuestion() (question string, answer int, err error) {
	a, err := randomInt(captchaMaxOperand)
	if err != nil {
		return "", 0, err
	}
	b, err := randomInt(captchaMaxOperand)
	if err != nil {
		return "", 0, err
	}
	subtract, err := randomInt(2)
	if err != nil {
		return "", 0, err
	}
	a, b = a+1, b+1
	if subtract == 1 {
		if a < b {
			a, b = b, a
		}
		return fmt.Sprintf("What is %d - %d?", a, b), a - b, nil
	}
	return fmt.Sprintf("What is %d + %d?", a, b), a + b, nil
}

// NewCaptcha creates a registration captcha: GET /v1/auth/captcha.
func (s *Service) NewCaptcha(ctx context.Context) (CaptchaChallenge, error) {
	question, answer, err := newCaptchaQuestion()
	if err != nil {
		return CaptchaChallenge{}, err
	}
	id, _, err := newToken()
	if err != nil {
		return CaptchaChallenge{}, err
	}
	now := s.clock()
	c := Captcha{ID: id, AnswerHash: captchaAnswerHash(id, answer), ExpiresAt: now.Add(CaptchaTTL), CreatedAt: now}
	if err := s.store.CreateCaptcha(ctx, c); err != nil {
		return CaptchaChallenge{}, fmt.Errorf("store captcha: %w", err)
	}
	return CaptchaChallenge{CaptchaID: id, Question: question, ExpiresAt: c.ExpiresAt}, nil
}

// parseCaptchaInput is the cheap check, made with the other fields and without
// touching the store: it returns the id and the answer as a number, or a
// message for the "captcha" field. Nothing is consumed here, so a typo in
// another field does not cost the person their question.
func parseCaptchaInput(in *CaptchaInput) (id string, answer int, msg string) {
	if in == nil {
		return "", 0, msgCaptchaRequired
	}
	id, text := strings.TrimSpace(in.CaptchaID), strings.TrimSpace(in.Answer)
	if id == "" || text == "" {
		return "", 0, msgCaptchaRequired
	}
	if !plausibleToken(id) {
		return "", 0, msgCaptchaExpired // cannot be one of ours
	}
	if len(text) > captchaMaxAnswerDigits {
		return "", 0, msgCaptchaWrong
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return "", 0, msgCaptchaWrong // only digits are an answer
		}
	}
	answer, err := strconv.Atoi(text)
	if err != nil {
		return "", 0, msgCaptchaWrong
	}
	return id, answer, ""
}

// checkCaptcha consumes the captcha and judges the answer. It returns "" when
// the answer is right, otherwise the message for the "captcha" field. The
// captcha is used up either way, so an answer cannot be found by retrying.
func (s *Service) checkCaptcha(ctx context.Context, id string, answer int, now time.Time) (string, error) {
	c, err := s.store.ConsumeCaptcha(ctx, id, now)
	switch {
	case errors.Is(err, ErrNotFound):
		return msgCaptchaExpired, nil // unknown or already used
	case err != nil:
		return "", fmt.Errorf("consume captcha: %w", err)
	case !now.Before(c.ExpiresAt):
		return msgCaptchaExpired, nil
	case subtle.ConstantTimeCompare(c.AnswerHash, captchaAnswerHash(id, answer)) != 1:
		return msgCaptchaWrong, nil
	}
	return "", nil
}
