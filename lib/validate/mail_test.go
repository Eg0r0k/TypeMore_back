package validate

import (
	"testing"
)


func TestEmail(t *testing.T){
	tests := []struct {
		email        string
		expected     string
		expectError  bool
}{
		{
				email:       "test@example.com",
				expected:    "test@example.com",
				expectError: false,
		},
		{
				email:       "invalid-email",
				expected:    "",
				expectError: true,
		},
		{
				email:       "user@invalid_domain",
				expected:    "",
				expectError: true,
		},
		{
				email:       "",
				expected:    "",
				expectError: true,
		},
		{
				email:       "user@.com",
				expected:    "",
				expectError: true,
		},
		{
			email:"user@dsasd@sd.com",
			expected: "",
			expectError: true,
		},
		{
			email: "sda.sdas@ds",
			expected: "",
			expectError: true,
		},
}
for _, tt := range tests {
	t.Run(tt.email, func(t *testing.T) {
			result, err := Email(tt.email)

			if (err != nil) != tt.expectError {
					t.Errorf("expected error: %v, got: %v", tt.expectError, err)
			}
			if result != tt.expected {
					t.Errorf("expected result: %s, got: %s", tt.expected, result)
			}
	})
}
}