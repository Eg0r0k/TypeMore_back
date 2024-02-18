import { IsEmail, IsNotEmpty, IsString, Length } from 'class-validator';

export class LoginUserDto {
  @IsEmail()
  @IsNotEmpty()
  email!: string;

  @IsString()
  @Length(6, 16)
  password_hash!: string;
}
