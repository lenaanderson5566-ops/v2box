package rate

import (
	"github.com/juju/ratelimit"
	"sync/atomic"
	"time"
)

type DynamicBucket struct {
	v atomic.Pointer[ratelimit.Bucket]
}

func NewDynamicBucket(rate int64) *DynamicBucket { d := &DynamicBucket{}; d.Update(rate); return d }
func (d *DynamicBucket) Get() *ratelimit.Bucket  { return d.v.Load() }
func (d *DynamicBucket) Update(rate int64) {
	d.v.Store(ratelimit.NewBucketWithQuantum(time.Second, rate, rate))
}
