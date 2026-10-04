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

// passwordResetEmail carries the reset link. It works once and expires in an hour.
func passwordResetEmail(locale, link string) (subject, text string) {
	if locale == "en" {
		return "Reset your Orion POS password",
			strings.Join([]string{
				"Someone, hopefully you, asked to reset the password of your Orion POS account.",
				"",
				"Open this link to choose a new password. It works once and expires in one hour:",
				link,
				"",
				"If you did not ask for this, ignore this email: your password has not changed.",
			}, "\n")
	}
	return "Atur ulang kata sandi Orion POS Anda",
		strings.Join([]string{
			"Seseorang, semoga Anda, meminta pengaturan ulang kata sandi akun Orion POS Anda.",
			"",
			"Buka tautan ini untuk memilih kata sandi baru. Tautan hanya berlaku sekali dan kedaluwarsa dalam satu jam:",
			link,
			"",
			"Jika Anda tidak memintanya, abaikan email ini: kata sandi Anda tidak berubah.",
		}, "\n")
}

// passwordChangedEmail tells the user their password changed. The link is the way back in if it
// was not them.
func passwordChangedEmail(locale, resetURL string) (subject, text string) {
	if locale == "en" {
		return "Your Orion POS password was changed",
			strings.Join([]string{
				"The password of your Orion POS account was just changed, and every device signed in with the old one was signed out.",
				"",
				"If this was you, there is nothing to do. If it was not, reset your password now:",
				resetURL,
			}, "\n")
	}
	return "Kata sandi Orion POS Anda telah diubah",
		strings.Join([]string{
			"Kata sandi akun Orion POS Anda baru saja diubah, dan semua perangkat yang masuk dengan kata sandi lama telah dikeluarkan.",
			"",
			"Jika ini Anda, tidak ada yang perlu dilakukan. Jika bukan, atur ulang kata sandi Anda sekarang:",
			resetURL,
		}, "\n")
}
