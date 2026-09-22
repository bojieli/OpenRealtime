"""SDPA bridge for ELLSA's joint speech/vision attention call convention."""

import torch
from torch.nn import functional as F


def joint_sdpa(self, query_states, key_states, value_states, attention_mask,
               query_length, dropout=0.0, softmax_scale=None):
    """Accept B,T,H,D tensors; preserve bottom-right causal cache alignment.

    ELLSA's SDPA model prepares a four-dimensional additive mask, but its
    joint attention still calls the FlashAttention method name. No weights or
    projections are changed by this bridge.
    """
    q, k, v = (x.transpose(1, 2) for x in (query_states, key_states, value_states))
    if query_length != q.shape[-2] or query_length > k.shape[-2]:
        raise ValueError("invalid joint-attention query/cache lengths")
    if q.shape[1] % k.shape[1]:
        raise ValueError("query heads must be divisible by KV heads")
    groups = q.shape[1] // k.shape[1]
    k, v = (x.repeat_interleave(groups, dim=1) for x in (k, v))
    keys = k.shape[-2]
    causal = torch.arange(keys, device=q.device)[None, :] <= (
        torch.arange(query_length, device=q.device)[:, None] + keys - query_length
    )
    mask = causal if self.is_causal else torch.ones_like(causal)
    query_padding = None
    if attention_mask is not None:
        if attention_mask.ndim == 2:
            mask = mask[None, None] & attention_mask[:, None, None, :].bool()
            query_padding = attention_mask[:, -query_length:].bool()
        elif attention_mask.ndim == 4:
            if attention_mask.dtype == torch.bool:
                mask = mask & attention_mask
            else:
                mask = attention_mask.masked_fill(~mask, float("-inf"))
        else:
            raise ValueError("joint attention expects a 2D or 4D mask")
    output = F.scaled_dot_product_attention(q, k, v, attn_mask=mask,
                                           dropout_p=dropout, scale=softmax_scale)
    output = output.transpose(1, 2)
    if query_padding is not None:
        output = output.masked_fill(~query_padding[:, :, None, None], 0)
    return output
