export function rowAt(offsets: number[], offset: number) {
  let low = 0
  let high = offsets.length - 1
  while (low < high) {
    const middle = (low + high) >>> 1
    if (offsets[middle + 1] <= offset) low = middle + 1
    else high = middle
  }
  return low
}
