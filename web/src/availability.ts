// The vocabulary a host uses to describe one of its models, and how each state
// is worded and coloured for a reader. A managed model is advertised as
// `unloaded` once its weights are downloaded and registered: the host has it,
// and loads it into the runner when the first request arrives. That is a
// usable model, not an absent one, so only `offline` reads as unavailable.

export type Availability = 'ready' | 'unloaded' | 'busy' | 'offline' | 'unavailable' | string

export const isUnavailable = (availability: Availability) =>
  availability === 'offline' || availability === 'unavailable'

export const availabilityLabel = (availability: Availability, isManaged = false) => {
  switch (availability) {
    case 'ready':
      return isManaged ? 'Loaded & ready' : 'Ready'
    case 'unloaded':
      return 'Starts on request'
    case 'busy':
      return isManaged ? 'Loaded · busy' : 'Busy'
    case 'offline':
    case 'unavailable':
      return 'Unavailable'
    default:
      return availability || 'Unknown'
  }
}

export const availabilityColor = (availability: Availability) => {
  if (availability === 'ready') return 'var(--accent-success)'
  if (availability === 'unloaded') return 'var(--accent-primary)'
  if (availability === 'busy') return '#f59e0b'
  return 'var(--border-color)'
}
