package search

import (
	"context"
	"errors"
	"testing"
)

func TestSynthesizesNontrivialExpression(t *testing.T) {
	// Hidden validation pairs are not part of the training-only search target.
	p:=Problem{Training:[]Example{{1,2,7},{2,3,13},{3,4,21}},
		Validation:[]Example{{4,5,31},{5,7,47}},
		Constants:[]int64{1,2},MaxDepth:3,MaxCandidates:25000}
	s,err:=Synthesize(context.Background(),p)
	if err!=nil{t.Fatal(err)}
	if s.Expression=="" || s.ValidationCount!=2 || s.Explored<1 {t.Fatalf("invalid solution: %+v",s)}
	again,err:=Synthesize(context.Background(),p)
	if err!=nil || again.Expression!=s.Expression {t.Fatalf("not deterministic: %+v / %v",again,err)}
}

func TestRejectsUnsolvableAndCanceled(t *testing.T) {
	p:=Problem{Training:[]Example{{1,1,999}},Validation:[]Example{{2,2,999}},MaxDepth:0,MaxCandidates:10}
	if _,err:=Synthesize(context.Background(),p);!errors.Is(err,ErrNoSolution){t.Fatal(err)}
	ctx,cancel:=context.WithCancel(context.Background());cancel()
	if _,err:=Synthesize(ctx,p);!errors.Is(err,context.Canceled){t.Fatal(err)}
	p.MaxCandidates=0
	if _,err:=Synthesize(context.Background(),p);!errors.Is(err,ErrInvalid){t.Fatal(err)}
}

func TestSearchBudget(t *testing.T) {
	p:=Problem{Training:[]Example{{1,1,1000}},Validation:[]Example{{2,2,1000}},MaxDepth:3,MaxCandidates:1}
	if _,err:=Synthesize(context.Background(),p);!errors.Is(err,ErrNoSolution){t.Fatal(err)}
}
