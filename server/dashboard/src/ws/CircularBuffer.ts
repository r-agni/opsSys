/** O(1) push circular buffer. snapshot() returns items in insertion order. */
export class CircularBuffer<T> {
  private readonly capacity: number
  private buf: (T | undefined)[]
  private head = 0
  private count = 0

  constructor(capacity: number) {
    this.capacity = capacity
    this.buf = new Array(capacity)
  }

  push(item: T): void {
    this.buf[this.head] = item
    this.head = (this.head + 1) % this.capacity
    if (this.count < this.capacity) this.count++
  }

  snapshot(): T[] {
    if (this.count === 0) return []
    const start = this.count < this.capacity ? 0 : this.head
    const result: T[] = new Array(this.count)
    for (let i = 0; i < this.count; i++) {
      result[i] = this.buf[(start + i) % this.capacity] as T
    }
    return result
  }

  get size(): number { return this.count }
  get latest(): T | undefined {
    if (this.count === 0) return undefined
    return this.buf[(this.head - 1 + this.capacity) % this.capacity]
  }
}
