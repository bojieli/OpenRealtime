"""Numerical joint-attention regression; run with the ELLSA venv."""
import types
import torch
from ellsa_attention import joint_sdpa
torch.manual_seed(42)
for qlen,klen in [(5,5),(1,7),(3,7)]:
 for kind in ['none','padding','additive']:
  q=torch.randn(2,qlen,4,8,dtype=torch.float64)
  k=torch.randn(2,klen,2,8,dtype=torch.float64)
  v=torch.randn_like(k)
  mask=None
  if kind=='padding':
   mask=torch.ones(2,klen,dtype=torch.bool);mask[:,0]=False
  if kind=='additive':
   mask=torch.zeros(2,1,qlen,klen,dtype=q.dtype);mask[:,:,:,0]=-torch.inf
  out=joint_sdpa(types.SimpleNamespace(is_causal=True),q,k,v,mask,qlen)
  expected=torch.zeros_like(out)
  for b in range(2):
   for t in range(qlen):
    for h in range(4):
     last=klen-qlen+t+1
     scores=k[b,:last,h//2]@q[b,t,h]/8**.5
     if kind!='none':scores[0]=-torch.inf
     weights=scores.softmax(0).nan_to_num()
     expected[b,t,h]=weights@v[b,:last,h//2]
    if kind=='padding' and not mask[b,klen-qlen+t]:expected[b,t]=0
  torch.testing.assert_close(out,expected)
print('9 numerical reference comparisons passed (GQA, prefill, cached decoding, padding/additive masks)')
