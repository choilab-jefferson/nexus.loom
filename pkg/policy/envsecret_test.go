package policy

import "testing"

func TestIsSecretEnvName(t *testing.T) {
	for _, n := range []string{"HF_TOKEN", "OPENAI_API_KEY", "API_KEY", "GITHUB_TOKEN", "DB_PASSWORD", "my_secret", "AWS_SECRET_ACCESS_KEY", "HUGGING_FACE_HUB_TOKEN"} {
		if !IsSecretEnvName(n) {
			t.Errorf("%s should be secret", n)
		}
	}
	for _, n := range []string{"PYTHONPATH", "CUDA_VISIBLE_DEVICES", "HF_HOME", "OMP_NUM_THREADS", "TRANSFORMERS_CACHE"} {
		if IsSecretEnvName(n) {
			t.Errorf("%s should not be secret", n)
		}
	}
}
