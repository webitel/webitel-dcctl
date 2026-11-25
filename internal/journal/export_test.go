package journal

// SetNewID replaces the id generator, to make keys collide.
func (j *Journal) SetNewID(f func() string) { j.newID = f }
