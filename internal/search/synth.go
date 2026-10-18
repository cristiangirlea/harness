// Package search implements bounded, deterministic program synthesis without an LLM.
package search

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

var ErrNoSolution = errors.New("no verified solution within search budget")
var ErrInvalid = errors.New("invalid synthesis problem")

// Example is an observed input/output pair for an integer expression.
type Example struct { X, Y, Want int64 }

// Problem defines the grammar and evidence used to search for an expression.
// Validation examples are held out of candidate selection.
type Problem struct {
	Training []Example
	Validation []Example
	Constants []int64
	MaxDepth int
	MaxCandidates int
}

// Solution contains a verified expression and search accounting.
type Solution struct {
	Expression string
	Depth int
	Explored int
	TrainingCount int
	ValidationCount int
}

type expr struct {
	text string
	depth int
	values []int64
	eval func(int64,int64)(int64,bool)
}

func add(a,b int64)(int64,bool) {
	c:=a+b
	return c,(b<=0 || c>=a)&&(b>=0 || c<=a)
}
func sub(a,b int64)(int64,bool) {
	c:=a-b
	return c,(b>=0 || c<=a)&&(b<=0 || c>=a)
}
func mul(a,b int64)(int64,bool) {
	if a==0 || b==0{return 0,true}
	c:=a*b
	return c,c/b==a && !(a == -1<<63 && b == -1) && !(b == -1<<63 && a == -1)
}
type operation struct { symbol string; apply func(int64,int64)(int64,bool) }

// Synthesize enumerates expressions built from x, y, constants, +, - and *.
// It returns only candidates that satisfy training and held-out validation.
func Synthesize(ctx context.Context,p Problem)(Solution,error) {
	if len(p.Training)==0 || len(p.Validation)==0 || p.MaxDepth<0 || p.MaxDepth>5 || p.MaxCandidates<1 {
		return Solution{},ErrInvalid
	}
	examples:=append([]Example(nil),p.Training...)
	p.Constants=append([]int64(nil),p.Constants...)
	sort.Slice(p.Constants,func(i,j int)bool{return p.Constants[i]<p.Constants[j]})
	seen:=map[string]bool{}
	all:=[]expr{}
	explored:=0
	key:=func(values []int64)string{return fmt.Sprint(values)}
	addExpr:=func(e expr) (bool,error) {
		if err:=ctx.Err();err!=nil{return false,err}
		if explored>=p.MaxCandidates{return false,ErrNoSolution}
		explored++
		e.values=make([]int64,len(examples))
		for i,v:=range examples {
			n,ok:=e.eval(v.X,v.Y)
			if !ok{return false,nil}
			e.values[i]=n
		}
		k:=key(e.values)
		ok:=true
		for i,v:=range examples {if e.values[i]!=v.Want {ok=false;break}}
		if ok {
			for _,v:=range p.Validation {
				n,valid:=e.eval(v.X,v.Y)
				if !valid || n!=v.Want {ok=false;break}
			}
		}
		if seen[k] { return ok,nil }
		seen[k]=true
		all=append(all,e)
		return ok,nil
	}
	base:=[]expr{
		{text:"x",eval:func(x,y int64)(int64,bool){return x,true}},
		{text:"y",eval:func(x,y int64)(int64,bool){return y,true}},
	}
	for _,c:=range p.Constants {v:=c;base=append(base,expr{text:fmt.Sprint(v),eval:func(x,y int64)(int64,bool){return v,true}})}
	for _,e:=range base {
		found,err:=addExpr(e)
		if found{return Solution{e.text,e.depth,explored,len(p.Training),len(p.Validation)},nil}
		if err!=nil{return Solution{},err}
	}
	ops:=[]operation{{"+",add},{"-",sub},{"*",mul}}
	for depth:=1;depth<=p.MaxDepth;depth++ {
		// Snapshot: newly generated expressions are considered in the next depth.
		previous:=append([]expr(nil),all...)
		for _,left:=range previous {
			for _,right:=range previous {
				if max(left.depth,right.depth)+1!=depth{continue}
				for _,op:=range ops {
					if op.symbol!="-" && left.text>right.text {continue}
					l,r,f:=left,right,op.apply
					e:=expr{text:"("+l.text+op.symbol+r.text+")",depth:depth,
						eval:func(x,y int64)(int64,bool){
							a,ok:=l.eval(x,y);if !ok{return 0,false}
							b,ok:=r.eval(x,y);if !ok{return 0,false}
							return f(a,b)
						},
					}
					found,err:=addExpr(e)
					if found{return Solution{e.text,depth,explored,len(p.Training),len(p.Validation)},nil}
					if err!=nil{return Solution{},err}
				}
			}
		}
	}
	return Solution{},ErrNoSolution
}

func max(a,b int)int{if a>b{return a};return b}
