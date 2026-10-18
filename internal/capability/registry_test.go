package capability

import (
 "context"
 "errors"
 "reflect"
 "testing"
)

func TestRegistryExecutesWithoutModel(t *testing.T) {
 r := NewRegistry()
 d := Descriptor{ID:"math.double",Version:"1",Domain:"math"}
 if err := r.Register(d,func(_ context.Context, in []byte)(Result,error){
  return Result{Output:append(append([]byte(nil),in...),in...)},nil
 }); err != nil { t.Fatal(err) }
 got,err := r.Execute(context.Background(),"math.double","1",Request{Input:[]byte("ab")})
 if err != nil || string(got.Output)!="abab" { t.Fatalf("got %q, %v",got.Output,err) }
 if len(r.List())!=1 { t.Fatal("missing registered capability") }
 if !errors.Is(r.Register(d,func(context.Context,[]byte)(Result,error){return Result{},nil}),ErrDuplicate) { t.Fatal("expected duplicate") }
}

func TestSideEffectPolicy(t *testing.T) {
 r:=NewRegistry()
 calls:=0
 d:=Descriptor{ID:"write",Version:"1",Domain:"filesystem",Effects:[]string{"filesystem.write"}}
 if err:=r.Register(d,func(context.Context,[]byte)(Result,error){calls++;return Result{},nil});err!=nil{t.Fatal(err)}
 if _,err:=r.Execute(context.Background(),"write","1",Request{});!errors.Is(err,ErrUnauthorized){t.Fatalf("expected denial: %v",err)}
 if calls!=0 {t.Fatal("unauthorized handler executed")}
 if _,err:=r.Execute(context.Background(),"write","1",Request{AllowedEffects:[]string{"filesystem.write"}});err!=nil{t.Fatal(err)}
 if calls!=1 {t.Fatal("handler not executed")}
 got:=r.List()
 got[0].Effects[0]="mutated"
 if !reflect.DeepEqual(r.List()[0].Effects,[]string{"filesystem.write"}) {t.Fatal("descriptor leaked mutable state")}
}

func TestCanceledAndMissing(t *testing.T) {
 r:=NewRegistry()
 if _,err:=r.Execute(context.Background(),"missing","1",Request{});!errors.Is(err,ErrNotFound){t.Fatal(err)}
 ctx,cancel:=context.WithCancel(context.Background());cancel()
 if _,err:=r.Execute(ctx,"missing","1",Request{});!errors.Is(err,context.Canceled){t.Fatal(err)}
 if err:=r.Register(Descriptor{},nil);!errors.Is(err,ErrInvalid){t.Fatal(err)}
}
