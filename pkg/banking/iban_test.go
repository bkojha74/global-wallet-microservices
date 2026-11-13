package banking

import (
	"testing"
)

func TestValidateIBAN(t *testing.T) {
	tests := []struct {
		name    string
		iban    string
		wantErr bool
	}{
		{
			name:    "Valid German IBAN",
			iban:    "DE89370400440532013000",
			wantErr: false,
		},
		{
			name:    "Valid German IBAN with spaces",
			iban:    "DE89 3704 0044 0532 0130 00",
			wantErr: false,
		},
		{
			name:    "Valid UK IBAN",
			iban:    "GB29NWBK60161331926819",
			wantErr: false,
		},
		{
			name:    "Invalid checksum",
			iban:    "DE89370400440532013001",
			wantErr: true,
		},
		{
			name:    "Invalid length for country (German 21 instead of 22)",
			iban:    "DE8937040044053201300",
			wantErr: true,
		},
		{
			name:    "Invalid format chars",
			iban:    "DE8937040044053201300!",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateIBAN(tt.iban)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateIBAN(%s) error = %v, wantErr %v", tt.iban, err, tt.wantErr)
			}
		})
	}
}

func TestGenerateAndValidateIBAN(t *testing.T) {
	// Generate valid US virtual IBAN: 20 chars BBAN + 4 chars US+digits = 24 chars
	bban := "CITI1234567890123456"
	iban, err := GenerateIBAN("US", bban)
	if err != nil {
		t.Fatalf("GenerateIBAN failed: %v", err)
	}

	if err := ValidateIBAN(iban); err != nil {
		t.Fatalf("Generated IBAN %s failed validation: %v", iban, err)
	}
}

func TestValidateBIC(t *testing.T) {
	tests := []struct {
		name    string
		bic     string
		wantErr bool
	}{
		{"Valid 8 chars", "DEUTDEDB", false},
		{"Valid 11 chars", "DEUTDEDBFRA", false},
		{"Valid with spaces", " DEUT DEDB ", false},
		{"Valid with alphanumeric location", "DEUTDE2X", false},
		{"Invalid length 7 chars", "DEUTDED", true},
		{"Invalid length 10 chars", "DEUTDEDBFR", true},
		{"Invalid country code digits", "DEUT1234", true},
		{"Invalid characters", "DEU!DEDB", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBIC(tt.bic)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateBIC(%s) error = %v, wantErr %v", tt.bic, err, tt.wantErr)
			}
		})
	}
}
