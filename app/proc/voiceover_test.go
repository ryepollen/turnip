package proc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVotArgsLivelyVoice(t *testing.T) {
	v := &VoiceoverService{OutputDir: "/out", TargetLang: "ru"}

	plain := v.votArgs("vo_x.mp3", "https://www.youtube.com/watch?v=x")
	assert.NotContains(t, plain, "--lively-voice", "no token — the old flat voices, flag absent")
	assert.Equal(t, "https://www.youtube.com/watch?v=x", plain[len(plain)-1], "URL stays last")

	v.LivelyToken = "y0_secret"
	lively := v.votArgs("vo_x.mp3", "https://www.youtube.com/watch?v=x")
	joined := strings.Join(lively, " ")
	assert.Contains(t, joined, "--lively-voice")
	assert.Contains(t, joined, "--api-token y0_secret")
	assert.Equal(t, "https://www.youtube.com/watch?v=x", lively[len(lively)-1], "URL stays last even with the flag pair")
}

func TestMaskVotArgsHidesToken(t *testing.T) {
	v := &VoiceoverService{OutputDir: "/out", TargetLang: "ru", LivelyToken: "y0_secret"}
	args := v.votArgs("vo_x.mp3", "u")

	masked := maskVotArgs(args)
	assert.NotContains(t, strings.Join(masked, " "), "y0_secret", "the token must never reach the logs")
	assert.Contains(t, strings.Join(masked, " "), "--api-token ***")
	assert.Contains(t, strings.Join(args, " "), "y0_secret", "the original args are untouched — vot-cli still gets the real token")
}
