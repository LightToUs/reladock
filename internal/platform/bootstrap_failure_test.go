package platform
import("context";"errors";"testing")
func TestBootstrapAuditFailureRollsBackAndNewGrantRevokesOld(t *testing.T){
 s:=testStore(t);ctx:=context.Background();who,_:=register(t,s,"bootstrap-failure@example.com");if err:=s.SetPlatformAdmin(ctx,who.Email,true);err!=nil{t.Fatal(err)}
 g,err:=s.CreateNodeBootstrap(ctx,who,"hk-01",1);if err!=nil{t.Fatal(err)}
 if _,err=s.CreateNodeBootstrap(ctx,who,"hk-01",1);!errors.Is(err,ErrConflict){t.Fatal("stale revision accepted")}
 next,err:=s.CreateNodeBootstrap(ctx,who,"hk-01",g.Revision);if err!=nil{t.Fatal(err)}
 if _,err=s.RedeemNodeBootstrap(ctx,g.Token,[]byte("csr"),func(string,[]byte)([]byte,error){t.Fatal("revoked token signed");return nil,nil});!errors.Is(err,ErrUnauthorized){t.Fatal(err)}
 if _,err=s.pool.Exec(ctx,"ALTER TABLE platform_audit_logs ADD CONSTRAINT reject_bootstrap CHECK (action <> 'node.bootstrap-redeem')");err!=nil{t.Fatal(err)}
 if _,err=s.RedeemNodeBootstrap(ctx,next.Token,[]byte("csr"),func(string,[]byte)([]byte,error){return []byte("certificate-fixture"),nil});err==nil{t.Fatal("ignored audit failure")}
 var fingerprint *string;var redeemed bool
 if err=s.pool.QueryRow(ctx,"SELECT certificate_fingerprint FROM nodes WHERE id='hk-01'").Scan(&fingerprint);err!=nil||fingerprint!=nil{t.Fatal("identity survived rollback")}
 if err=s.pool.QueryRow(ctx,"SELECT csr_hash IS NOT NULL FROM node_bootstrap WHERE token_hash=$1",bootstrapHash([]byte(next.Token))).Scan(&redeemed);err!=nil||redeemed{t.Fatal("grant consumed on failure")}
}
