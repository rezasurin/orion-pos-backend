package identity

import (
	"fmt"
	"strings"
)

// verificationEmail renders the verification message in the user's language. The link is the only
// thing in the message that matters, so the text is plain and short.
func verificationEmail(locale, link string) (subject, text string) {
	if locale == "en" {
		return "Verify your email address for Orion POS",
			strings.Join([]string{
				"Welcome to Orion POS.",
				"",
				"Open this link to verify your email address. It works once and expires in 24 hours:",
				link,
				"",
				"If you did not create an account, ignore this email.",
			}, "\n")
	}
	return "Verifikasi alamat email Anda untuk Orion POS",
		strings.Join([]string{
			"Selamat datang di Orion POS.",
			"",
			"Buka tautan ini untuk memverifikasi alamat email Anda. Tautan hanya berlaku sekali dan kedaluwarsa dalam 24 jam:",
			link,
			"",
			"Jika Anda tidak membuat akun, abaikan email ini.",
		}, "\n")
}

func verificationLink(baseURL, token string) string {
	return fmt.Sprintf("%s/verify-email?token=%s", strings.TrimRight(baseURL, "/"), token)
}

// accountExistsEmail is sent when someone signs up with an address that already has an account. It
// is the same whether or not the owner was the one who tried, and it never says more to the
// website than the signup response did.
func accountExistsEmail(locale, loginURL string) (subject, text string) {
	if locale == "en" {
		return "Someone tried to create an Orion POS account with your email address",
			strings.Join([]string{
				"Someone, possibly you, tried to sign up for Orion POS with this email address.",
				"",
				"You already have an account, so nothing was created. Sign in here:",
				loginURL,
				"",
				"If this was not you, you can ignore this email.",
			}, "\n")
	}
	return "Seseorang mencoba membuat akun Orion POS dengan alamat email Anda",
		strings.Join([]string{
			"Seseorang, mungkin Anda, mencoba mendaftar di Orion POS dengan alamat email ini.",
			"",
			"Anda sudah memiliki akun, jadi tidak ada akun baru yang dibuat. Masuk di sini:",
			loginURL,
			"",
			"Jika ini bukan Anda, abaikan email ini.",
		}, "\n")
}
