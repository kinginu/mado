package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Answering Claude Code's AskUserQuestion from mado (DESIGN §21).

// ToolAskUserQuestion is the tool whose approval is a question card.
const ToolAskUserQuestion = "AskUserQuestion"

const (
	// reasonQuestion is the auto mode reason of a question: never decided by policy.
	reasonQuestion = "a question for you"
	// dismissMessage is what claude reads when the user dismisses the card.
	dismissMessage = "The user dismissed the question in mado."
	// maxAnswer bounds one answer and one note, in bytes.
	maxAnswer = 2000
)

type askOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Preview     string `json:"preview,omitempty"`
}

type askQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header,omitempty"`
	Options     []askOption `json:"options,omitempty"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
}

// storedAnswers is the approvals.answers column.
type storedAnswers struct {
	Answers map[string]string `json:"answers"`
	Notes   map[string]string `json:"notes,omitempty"`
}

// askQuestions reads the questions of an AskUserQuestion input.
func askQuestions(input string) ([]askQuestion, error) {
	var in struct {
		Questions []askQuestion `json:"questions"`
	}
	if err := json.Unmarshal([]byte(input), &in); err != nil || len(in.Questions) == 0 {
		return nil, errors.New("the question input has no questions")
	}
	return in.Questions, nil
}

// answerQuestion validates the user's answers and notes against the stored input and
// returns the updated_input for claude (the input plus answers, and annotations with
// the notes and the selected option's preview, as Claude Code's own dialog sends) and
// the JSON for the answers column.
func answerQuestion(input string, answers, notes map[string]string) (json.RawMessage, string, error) {
	qs, err := askQuestions(input)
	if err != nil {
		return nil, "", err
	}
	byText := map[string]askQuestion{}
	for _, q := range qs {
		byText[q.Question] = q
	}
	if len(answers) == 0 {
		return nil, "", errors.New("answers are required to answer a question")
	}
	clean := map[string]string{}
	for k, v := range answers {
		if _, ok := byText[k]; !ok {
			return nil, "", fmt.Errorf("%q is not one of the questions", k)
		}
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, "", fmt.Errorf("the answer to %q is empty", k)
		}
		if len(v) > maxAnswer {
			return nil, "", fmt.Errorf("the answer to %q is longer than %d bytes", k, maxAnswer)
		}
		clean[k] = v
	}
	cleanNotes := map[string]string{}
	for k, v := range notes {
		if _, ok := byText[k]; !ok {
			return nil, "", fmt.Errorf("%q is not one of the questions", k)
		}
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		if len(v) > maxAnswer {
			return nil, "", fmt.Errorf("the notes on %q are longer than %d bytes", k, maxAnswer)
		}
		cleanNotes[k] = v
	}
	annotations := map[string]map[string]string{}
	for k, v := range clean {
		q := byText[k]
		if q.MultiSelect {
			continue
		}
		for _, o := range q.Options {
			if o.Label == v && o.Preview != "" {
				annotations[k] = map[string]string{"preview": o.Preview}
			}
		}
	}
	for k, v := range cleanNotes {
		if annotations[k] == nil {
			annotations[k] = map[string]string{}
		}
		annotations[k]["notes"] = v
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return nil, "", errors.New("the question input is not an object")
	}
	in["answers"], _ = json.Marshal(clean)
	delete(in, "annotations")
	if len(annotations) > 0 {
		in["annotations"], _ = json.Marshal(annotations)
	}
	updated, err := json.Marshal(in)
	if err != nil {
		return nil, "", err
	}
	sa := storedAnswers{Answers: clean}
	if len(cleanNotes) > 0 {
		sa.Notes = cleanNotes
	}
	stored, _ := json.Marshal(sa)
	return updated, string(stored), nil
}
