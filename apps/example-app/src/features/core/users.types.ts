export interface IUser {
  id: string;
  name: string;
  role: 'admin' | 'user';
}

export interface ICreateUserDto {
  name: string;
  role?: 'admin' | 'user';
}

export interface IUpdateUserDto {
  name?: string;
  role?: 'admin' | 'user';
}
